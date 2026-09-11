// Command spoloh announces air raid alerts and scheduled events over a PA
// system attached to a single-board computer.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
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
	"github.com/b-ovsepian/spoloh/internal/control"
	"github.com/b-ovsepian/spoloh/internal/engine"
	"github.com/b-ovsepian/spoloh/internal/eventlog"
	"github.com/b-ovsepian/spoloh/internal/library"
	"github.com/b-ovsepian/spoloh/internal/netmgr"
	"github.com/b-ovsepian/spoloh/internal/player"
	"github.com/b-ovsepian/spoloh/internal/timedate"
	"github.com/b-ovsepian/spoloh/internal/watchdog"
	"github.com/b-ovsepian/spoloh/internal/web"
	"github.com/b-ovsepian/spoloh/internal/webhook"
)

// version is overwritten at build time with -ldflags.
var version = "dev"

// arch is the release artifact this binary was built as, stamped by the
// Makefile. "dev" means a local build that was never published, which the
// updater refuses to update — see internal/updater.
var arch = "dev"

const logCapacity = 2000

func main() {
	// Dispatched before flag.Parse: the daemon's own flags are defined below
	// and a subcommand must not be mistaken for one of them.
	switch cmd := subcommand(os.Args); {
	case cmd == "":
	case cmd == "update":
		os.Exit(runUpdate(os.Args[2:], os.Stdout, &http.Client{Timeout: 5 * time.Minute}))
	case control.IsCommand(cmd):
		// Everything else in this switch runs in the daemon's own process.
		// These talk to a daemon that is already running, over the unix
		// socket it listens on, and exit — see internal/control.
		os.Exit(control.Run(cmd, os.Args[2:], os.Stdout))
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q; the commands are:\n%s\n", os.Args[1], control.Usage)
		os.Exit(2)
	}

	configPath := flag.String("config", "/etc/spoloh/config.json", "path to config.json")
	dataDir := flag.String("data", "/var/lib/spoloh", "data directory for sounds, state, and the log")
	// The listen address is a flag, not a config.json field: a bad edit to
	// the config file must never be able to make the dashboard unreachable
	// with no recovery path short of pulling the SD card. CAP_NET_BIND_SERVICE
	// in deploy/spoloh.service is what lets this default bind :80 without root.
	listen := flag.String("listen", ":80", "address the dashboard listens on")
	// A flag for the same reason -listen is one: the path the daemon
	// publishes and the path the CLI dials have to agree, and the unit file
	// is where that agreement is visible and editable without a rebuild.
	socket := flag.String("socket", control.DefaultSocket, "path of the control socket the command line connects to")
	// Answered from this binary, without a daemon: `spoloh version` asks the
	// running daemon what it is, which is the different and usually more
	// useful question, but it needs the daemon to be up to answer at all.
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}
	if err := run(*configPath, *dataDir, *listen, *socket); err != nil {
		slog.Error("spoloh stopped", "err", err)
		os.Exit(1)
	}
}

func run(configPath, dataDir, listen, socket string) error {
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

	// Quiet hours can be configured to cover air raid alerts — the operator
	// asked for it, /events makes it deliberate, and every suppression lands
	// in the event log. It is still the one configuration that makes this
	// device choose silence during a raid, so the journal names it once at
	// boot, where whoever is debugging a quiet night starts looking.
	//
	// SilencesAlerts, not a bare Contains on applies_to: a category list
	// naming alerts on a disabled window silences nothing, and warning about
	// it would teach the reader to skip this line. Warn rather than Info —
	// legitimate, but never routine.
	if cfg.QuietHours.SilencesAlerts() {
		logger.Warn("quiet hours cover air raid alerts; between these hours this device will not sound an alert",
			"from", cfg.QuietHours.From, "to", cfg.QuietHours.To)
	}

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

	// alsa is named rather than built inline so /audio's device list (Deps.
	// Devices below) shares the exact value playback runs through, instead
	// of a second, pointlessly separate ALSA{} construction.
	alsa := player.ALSA{}
	pl := player.New(alsa, cfg.Audio.Device)
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

	// The dispatcher reads its webhooks from the live configuration on every
	// trigger, so one edited on the dashboard takes effect at the next
	// announcement rather than at the next restart. It writes its outcomes to
	// the same event log everything else here does; a nil log swallows them,
	// exactly as it does for the engine.
	hooks := webhook.New(cfgFn, lg, logger)

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
		// Dispatch returns before a connection is made — see
		// internal/webhook. Nothing on the announcement path waits for a
		// box that may be unplugged.
		Webhooks: hooks.Dispatch,
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
	// device to the site's network, and the supervisor stands down while it is
	// held. Without it the supervisor raises an access point on top of the
	// join — mid-join the radio reports neither connected nor hotspot-up,
	// which past the grace period is precisely the state it acts on. See
	// netmgr.JoinGate.
	joinGate := &netmgr.JoinGate{}
	sup := netmgr.NewSupervisor(netManager, cfgFn, clock.System{Loc: loc}, logger, joinGate)

	// timeManager is the write side of the tool clock.Timedatectl above
	// reads: the /time page sets the zone, the clock and the NTP switch
	// through it. A value type for the same reason NMCLI is one — it holds
	// no state and each call is an independent process.
	//
	// ServerFile follows -config rather than being hard-coded, so a run
	// against a config elsewhere (an e2e run, a manual invocation) writes its
	// NTP server beside that config instead of into a deployed device's
	// /etc/spoloh. On a real install the two are the same path, which is
	// what deploy/spoloh-timesync.service reads.
	timeManager := timedate.Timedatectl{
		ServerFile: filepath.Join(filepath.Dir(configPath), "ntp-server"),
	}

	websrv, err := web.New(web.Deps{
		Config:   store,
		Engine:   eng,
		Log:      lg,
		Player:   pl,
		Library:  lib,
		Network:  netManager,
		JoinGate: joinGate,
		Devices:  alsa,
		Timedate: timeManager,
		Logger:   logger,
		// Without this the footer on every page reads "збірка для
		// розробки" on a device running a stamped release — a visible
		// untruth, and the one place an operator looks to answer "what is
		// this box actually running" before reporting a fault.
		Version: version,
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
	players.Add(4)
	go func() {
		defer players.Done()
		pl.Run(ctx)
	}()
	go func() {
		defer players.Done()
		serveDashboard(ctx, func(ctx context.Context) error {
			return websrv.ListenAndServe(ctx, listen)
		}, logger)
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
	go func() {
		defer players.Done()
		// The control socket joins the same WaitGroup for the reason the
		// dashboard does: its commands reach the engine, the player and the
		// event log, so it has to be confirmed stopped before lg.Close().
		serveControl(ctx, socket, controlDevice{engine: eng, lib: lib, log: lg, cfg: cfgFn}, logger)
	}()

	logger.Info("spoloh started",
		"version", version,
		"region", cfg.Alerts.Region,
		"sounds", soundsDir,
		"audio_device", cfg.Audio.Device,
		"listen", listen,
		"socket", socket,
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

// firstRetry and maxRetry bound how hard serveWithRetry tries. One second so
// a port released a moment later is picked up almost at once; thirty so a
// subsystem that is never coming back costs two log lines a minute rather
// than a thousand.
const (
	firstRetry = 1 * time.Second
	maxRetry   = 30 * time.Second
)

// serveDashboard runs the dashboard, and keeps running it.
//
// A dashboard that fails must not take the daemon down — announcing alerts
// is the job, and serving a web page is not — but it must not be given up
// on either. Observed on a real board: a stale instance still held the
// port, the new one logged "bind: address already in use" at Error, and
// then went on polling sources and standing ready to sound a siren with no
// dashboard at all. The device looked alive and was unmanageable, and the
// one line explaining why was in a log reachable only over SSH — which is
// exactly the tool this product exists so nobody needs.
//
// Retrying is what the rest of this daemon already does with anything that
// can fail transiently: the alert source cascade, the network supervisor's
// back-off, the dashboard's own probe-before-reload. A held port is almost
// always transient. Exiting instead was considered and rejected: it trades
// a working siren for a clearer systemd status, and the siren is the
// product.
func serveDashboard(ctx context.Context, serve func(context.Context) error, logger *slog.Logger) {
	serveWithRetry(ctx, "dashboard", serve, logger)
}

// serveWithRetry is the shape above, generalised for the second subsystem
// that has the same obligation: the control socket. name is what the log
// lines call it, so an operator reading the journal can tell which one is
// failing without matching error text.
func serveWithRetry(ctx context.Context, name string, serve func(context.Context) error, logger *slog.Logger) {
	delay := firstRetry
	for {
		err := serve(ctx)
		switch {
		case err == nil:
			return
		case ctx.Err() != nil:
			// ctx was already cancelled when serve returned, so this is the
			// shutdown backstop firing, not a failure the operator needs to
			// act on — logging it at Error would train whoever reads these
			// logs to ignore the one line that means the subsystem could
			// not come up at all.
			logger.Warn(name+" did not shut down cleanly", "err", err)
			return
		}

		logger.Error(name+" stopped, retrying", "err", err, "retry_in", delay)
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		// Only ever grows on consecutive failures, so a subsystem that
		// served for months and then failed once still retries in a second.
		delay = min(delay*2, maxRetry)
	}
}
