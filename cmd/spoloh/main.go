// Command spoloh announces air raid alerts and scheduled events over a PA
// system attached to a single-board computer.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	// Embeds the timezone database so Europe/Kyiv resolves on a minimal OS
	// image that has no tzdata package installed.
	_ "time/tzdata"

	"github.com/b-ovsepian/spoloh/internal/alertsource"
	"github.com/b-ovsepian/spoloh/internal/alertstate"
	"github.com/b-ovsepian/spoloh/internal/clock"
	"github.com/b-ovsepian/spoloh/internal/configstore"
	"github.com/b-ovsepian/spoloh/internal/engine"
	"github.com/b-ovsepian/spoloh/internal/eventlog"
	"github.com/b-ovsepian/spoloh/internal/library"
	"github.com/b-ovsepian/spoloh/internal/netmgr"
	"github.com/b-ovsepian/spoloh/internal/player"
	"github.com/b-ovsepian/spoloh/internal/watchdog"
	"github.com/b-ovsepian/spoloh/internal/web"
)

// version is overwritten at build time with -ldflags.
var version = "dev"

const logCapacity = 2000

func main() {
	configPath := flag.String("config", "/etc/spoloh/config.json", "path to config.json")
	dataDir := flag.String("data", "/var/lib/spoloh", "data directory for sounds, state, and the log")
	// The listen address is a flag, not a config.json field: a bad edit to
	// the config file must never be able to make the dashboard unreachable
	// with no recovery path short of pulling the SD card. CAP_NET_BIND_SERVICE
	// in deploy/spoloh.service is what lets this default bind :80 without root.
	listen := flag.String("listen", ":80", "address the dashboard listens on")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}
	if err := run(*configPath, *dataDir, *listen); err != nil {
		slog.Error("spoloh stopped", "err", err)
		os.Exit(1)
	}
}

func run(configPath, dataDir, listen string) error {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	// configstore.Open seeds config.json with the defaults when it does not
	// exist yet, the same behaviour main.go used to implement by hand. The
	// parent directory still has to exist first: Open's underlying Save
	// writes atomically via a temp file in the same directory, which fails
	// outright if that directory is missing. In production
	// deploy/spoloh.tmpfiles creates /etc/spoloh before the unit ever starts;
	// this Mkdir is the same defensive fallback the previous implementation
	// had, kept for any other configPath (a fresh e2e run, a manual invocation).
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		return err
	}
	store, err := configstore.Open(configPath)
	if err != nil {
		return err
	}

	// The password has to exist before the supervisor's first tick can raise
	// a hotspot with it — a device installed before Plan 3b's installer
	// exists to generate and print one at install time still needs to reach
	// this on first boot. hadAPPassword is read before EnsureAPPassword so
	// the log line below fires only the one time the field was actually
	// filled in, never on a boot that finds it already set.
	hadAPPassword := store.Get().Network.APPassword != ""
	apPassword, err := netmgr.EnsureAPPassword(store)
	switch {
	case err != nil:
		// Logged and survived, not returned. Returning here becomes
		// os.Exit(1), and deploy/spoloh.service sets StartLimitIntervalSec=0,
		// so a board that cannot write its configuration — a read-only SD
		// card, an /etc/spoloh still owned by root because the installer
		// never ran systemd-tmpfiles --create — would crash-loop forever
		// over a password that exists only to serve networking. Announcing
		// alerts is the job; every other startup dependency in this function
		// already warns and carries on (the event log, the watchdog, the
		// dashboard), and the unit file refuses Requires=NetworkManager.service
		// for this same reason.
		//
		// The cost is named rather than hidden: the password stays empty, so
		// StartHotspot refuses to raise the access point (netmgr's
		// errEmptyAPPassword — an open one is worse than none) and the device
		// cannot be configured over Wi-Fi until someone fixes the write. That
		// is the trade, plainly: a device that cannot raise a setup access
		// point still announces air raid alerts, and a device that will not
		// start does not.
		logger.Error("could not store the access point password; the device will not be able to raise its setup access point", "err", err)
	case !hadAPPassword:
		// The one place this password is ever logged. Everywhere else in
		// this branch treats it as a credential that must never be logged,
		// formatted into an error, or put in a URL — internal/netmgr was
		// hardened twice for exactly that. This line exists only because the
		// alternative is worse: the Plan 3b installer that will print it and
		// the Task 4 dashboard page that will show it do not exist yet, and a
		// generated password nobody can read leaves the device unjoinable
		// with no way back short of a site visit. Delete this once both of
		// those land.
		logger.Info("generated the access point password", "password", apPassword)
	}

	// cfgFn is handed to the engine so every tick reads the live
	// configuration; cfg below is a single snapshot used only to set up
	// values fixed for the life of this process (the timezone, the initial
	// audio device, the startup log line).
	cfgFn := store.Get
	cfg := cfgFn()

	loc, err := time.LoadLocation(cfg.Device.Timezone)
	if err != nil {
		return fmt.Errorf("timezone %q: %w", cfg.Device.Timezone, err)
	}

	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return err
	}
	soundsDir := filepath.Join(dataDir, "sounds")
	lib := library.New(soundsDir, library.FFmpeg{})
	if err := lib.EnsureDefaults(); err != nil {
		return fmt.Errorf("install default sounds: %w", err)
	}

	// A SQLite file corrupted by a power cut, or an SD card the kernel has
	// remounted read-only — the classic Pi failure — must not crash-loop the
	// daemon: it can still poll upstream and still sound the siren from files
	// already on disk. The event log is a diagnostic subsystem and must never
	// be the reason the device is silent. A nil *eventlog.Log is valid and
	// does nothing, which is exactly what every consumer wants here.
	lg, err := eventlog.Open(filepath.Join(dataDir, "spoloh.db"), logCapacity)
	if err != nil {
		logger.Warn("could not open the event log; continuing without it", "err", err)
		lg = nil
	}
	defer lg.Close()

	// eventlog.KindConfigChange is written from exactly one place: the store
	// itself, for every successful update. Setting the administrator password
	// is the first configuration change any device sees, and until now it left
	// no trace at all. A nil *eventlog.Log swallows this, like every other
	// append.
	store.SetOnChange(func() {
		if err := lg.Append(eventlog.Entry{Kind: eventlog.KindConfigChange, OK: true}); err != nil {
			logger.Warn("could not record the configuration change", "err", err)
		}
	})

	pl := player.New(player.ALSA{}, cfg.Audio.Device)
	pl.OnError = func(r player.Request, stage string, err error) {
		logger.Error("playback failed", "stage", stage, "event", r.EventID, "err", err)
		_ = lg.Append(eventlog.Entry{
			Kind: eventlog.KindPlaybackError, EventID: r.EventID,
			Sound: filepath.Base(r.Path), OK: false, Err: err.Error(),
		})
	}

	machine := alertstate.New()
	statePath := filepath.Join(dataDir, "state.json")
	p, err := alertstate.LoadPersisted(statePath)
	if err != nil {
		logger.Warn("could not read the saved alert state", "path", statePath, "err", err)
	}
	// The configured oblast is passed in so a state file recorded for a
	// different one is discarded rather than trusted: /network can change the
	// oblast between two runs of this daemon. See alertstate.Machine.Restore.
	machine.Restore(p, cfg.Alerts.Region)

	wd, err := watchdog.Open()
	if err != nil {
		logger.Warn("could not open the systemd notify socket", "err", err)
	}
	defer wd.Close()

	eng := engine.New(engine.Deps{
		Config:    cfgFn,
		StatePath: statePath,
		Source:    alertsource.New(alertsource.DefaultBaseURL, cfg.Alerts.Sources, nil),
		Machine:   machine,
		Player:    pl,
		Library:   lib,
		Log:       lg,
		Clock:     clock.System{Loc: loc},
		Sync:      &clock.Timedatectl{},
		Logger:    logger,
		Watchdog:  wd,
	})

	// netManager drives NetworkManager through nmcli. A value type: see
	// NMCLI's doc comment for why it needs no construction. It is shared
	// between two callers with different rhythms — the supervisor below
	// polls it once every tickInterval, and the dashboard's /network page
	// (through the Network field passed into web.Deps below) calls it
	// directly whenever an operator asks for a scan or a connect. Sharing one
	// value is safe on our side and that is all this claims: NMCLI holds no
	// state, and each call is an independent process, so there is nothing
	// here for two goroutines to race on. What NetworkManager does when two
	// nmcli invocations arrive at once is its business, not something this
	// code establishes — the worst case is one of them failing, which both
	// callers already handle as an ordinary error.
	netManager := netmgr.NMCLI{}
	// One gate, two holders: the /network page takes it while it joins the
	// device to a school network, and the supervisor stands down while it is
	// held. Without it the supervisor raises an access point on top of the
	// join — mid-join the radio reports neither connected nor hotspot-up,
	// which past the grace period is precisely the state it acts on. See
	// netmgr.JoinGate.
	joinGate := &netmgr.JoinGate{}
	sup := netmgr.NewSupervisor(netManager, cfgFn, clock.System{Loc: loc}, logger, joinGate)

	websrv, err := web.New(web.Deps{
		Config:   store,
		Engine:   eng,
		Log:      lg,
		Player:   pl,
		Library:  lib,
		Network:  netManager,
		JoinGate: joinGate,
		Logger:   logger,
	})
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// The player runs in its own goroutine and its OnError writes to the
	// event log, so the log has to outlive it. Without this wait, the
	// deferred lg.Close() below could run while an OnError was mid-Append.
	// The dashboard server joins the same WaitGroup rather than getting a
	// second one: its handlers write to the same log (manual play/stop) and
	// read the same engine and player, so it has to be confirmed stopped
	// before lg.Close() for exactly the reason the player already was. The
	// network supervisor joins it too, for a plainer reason: it touches
	// neither the log nor anything else that outlives it, but the daemon
	// has exactly one shutdown mechanism, and giving the one goroutine that
	// keeps the device reachable a mechanism of its own would just be a
	// second thing to get wrong for no benefit.
	//
	// The order the spec cares about — HTTP server, then the engine, then
	// the player, then the log — is what this achieves even though all
	// three see ctx cancelled at once and eng.Run below can return while
	// ListenAndServe is still draining an in-flight request within its own
	// five-second grace period: ListenAndServe's own shutdown does not
	// return until its handlers have — except when the five-second backstop
	// in server.go fires and Close() gives up on them, which is the exception
	// named below — and players.Wait() below does not
	// return until ListenAndServe has, so nothing a *handler* can reach
	// (Engine, Player, the event log) is torn down while that handler is
	// still running, whichever of the three goroutines finishes first.
	//
	// One thing escapes that, and it is worth naming rather than leaving as
	// an implied guarantee: web.handleNetworkConnect detaches its join from
	// the request context on purpose (see its comment), so the join — and
	// the Config.Update that follows a successful one — can outlive both the
	// handler and lg.Close(). The store's OnChange hook then appends to a
	// closed event log. That is fine as behaviour, not as a claim:
	// database/sql returns an error rather than panicking on a closed
	// database, and the hook installed above logs it. Nothing else in this
	// codebase outlives ListenAndServe this way; a second thing that did
	// would need this paragraph re-examined.
	var players sync.WaitGroup
	players.Add(3)
	go func() {
		defer players.Done()
		pl.Run(ctx)
	}()
	go func() {
		defer players.Done()
		// A dashboard that fails to bind — or that stops for any other
		// reason — must not take the daemon down: announcing alerts is the
		// job, and serving a web page is not.
		err := websrv.ListenAndServe(ctx, listen)
		switch {
		case err == nil:
			// A clean stop: nothing to tell the operator.
		case ctx.Err() != nil:
			// ctx was already cancelled when ListenAndServe returned, so
			// this is the shutdown backstop firing (or some other hiccup
			// during a stop the daemon itself requested), not a failure the
			// operator needs to act on — logging it at Error would train
			// whoever reads these logs to ignore the one line that means
			// the dashboard could not come up at all.
			logger.Warn("dashboard did not shut down cleanly", "err", err)
		default:
			// The dashboard failed on its own — most likely a bind
			// failure at startup — while the daemon was not asking it to
			// stop. This is the one an operator should see.
			logger.Error("dashboard stopped", "err", err)
		}
	}()
	go func() {
		defer players.Done()
		// Same degradation contract as the player and the dashboard: a
		// board with no NetworkManager, or one whose nmcli wedges, must
		// still announce alerts. Every tick bounds its own nmcli work
		// (netmgr.tickDeadline) and an unreadable status changes nothing
		// (see Supervisor.Tick), so there is nothing here to guard against
		// beyond running it alongside the rest.
		sup.Run(ctx)
	}()

	logger.Info("spoloh started",
		"version", version,
		"region", cfg.Alerts.Region,
		"sounds", soundsDir,
		"audio_device", cfg.Audio.Device,
		"listen", listen,
	)
	wd.Ready()

	err = eng.Run(ctx)
	wd.Stopping()

	// stop() before the wait, not just deferred: pl.Run and ListenAndServe
	// both return on context cancellation, so cancelling here is what
	// guarantees the wait ends even if eng.Run returned for some reason
	// other than the context.
	stop()
	players.Wait()

	if err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	logger.Info("spoloh stopped cleanly")
	return nil
}
