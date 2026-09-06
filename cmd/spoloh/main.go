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
	"syscall"
	"time"

	// Embeds the timezone database so Europe/Kyiv resolves on a minimal OS
	// image that has no tzdata package installed.
	_ "time/tzdata"

	"github.com/spoloh/spoloh/internal/alertsource"
	"github.com/spoloh/spoloh/internal/alertstate"
	"github.com/spoloh/spoloh/internal/clock"
	"github.com/spoloh/spoloh/internal/config"
	"github.com/spoloh/spoloh/internal/engine"
	"github.com/spoloh/spoloh/internal/eventlog"
	"github.com/spoloh/spoloh/internal/library"
	"github.com/spoloh/spoloh/internal/player"
)

// version is overwritten at build time with -ldflags.
var version = "dev"

const logCapacity = 2000

func main() {
	configPath := flag.String("config", "/etc/spoloh/config.json", "path to config.json")
	dataDir := flag.String("data", "/var/lib/spoloh", "data directory for sounds, state, and the log")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}
	if err := run(*configPath, *dataDir); err != nil {
		slog.Error("spoloh stopped", "err", err)
		os.Exit(1)
	}
}

func run(configPath, dataDir string) error {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	cfg, err := config.Load(configPath)
	switch {
	case os.IsNotExist(err):
		cfg = config.Default()
		if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
			return err
		}
		if err := config.Save(configPath, cfg); err != nil {
			return err
		}
		logger.Info("wrote a default config", "path", configPath)
	case err != nil:
		return err
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

	lg, err := eventlog.Open(filepath.Join(dataDir, "spoloh.db"), logCapacity)
	if err != nil {
		return err
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

	eng := engine.New(engine.Deps{
		Config:    func() *config.Config { return cfg },
		StatePath: statePath,
		Source:    alertsource.New(alertsource.DefaultBaseURL, cfg.Alerts.Sources, nil),
		Machine:   machine,
		Player:    pl,
		Library:   lib,
		Log:       lg,
		Clock:     clock.System{Loc: loc},
		Sync:      clock.Timedatectl{},
		Logger:    logger,
	})

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go pl.Run(ctx)
	logger.Info("spoloh started",
		"version", version,
		"region", cfg.Alerts.Region,
		"sounds", soundsDir,
		"audio_device", cfg.Audio.Device,
	)

	if err := eng.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	logger.Info("spoloh stopped cleanly")
	return nil
}
