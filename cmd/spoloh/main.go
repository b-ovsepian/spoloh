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
	machine.Restore(p)

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

	websrv, err := web.New(web.Deps{
		Config:  store,
		Engine:  eng,
		Log:     lg,
		Player:  pl,
		Library: lib,
		Logger:  logger,
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
	// before lg.Close() for exactly the reason the player already was.
	//
	// The order the spec cares about — HTTP server, then the engine, then
	// the player, then the log — is what this achieves even though all
	// three see ctx cancelled at once and eng.Run below can return while
	// ListenAndServe is still draining an in-flight request within its own
	// five-second grace period: nothing that draining request can reach
	// (Engine, Player, the event log) is torn down or closed until
	// players.Wait() below confirms both goroutines have actually returned,
	// so the "stops first" that matters — no handler still running once the
	// log is allowed to close — holds regardless of which of the three
	// happens to return first.
	var players sync.WaitGroup
	players.Add(2)
	go func() {
		defer players.Done()
		pl.Run(ctx)
	}()
	go func() {
		defer players.Done()
		// A dashboard that fails to bind — or that stops for any other
		// reason — must not take the daemon down: announcing alerts is the
		// job, and serving a web page is not.
		if err := websrv.ListenAndServe(ctx, listen); err != nil {
			logger.Error("dashboard stopped", "err", err)
		}
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
