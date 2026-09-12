package main

import (
	"context"
	"log/slog"
	"path/filepath"

	"github.com/b-ovsepian/spoloh/internal/config"
	"github.com/b-ovsepian/spoloh/internal/control"
	"github.com/b-ovsepian/spoloh/internal/engine"
	"github.com/b-ovsepian/spoloh/internal/eventlog"
	"github.com/b-ovsepian/spoloh/internal/library"
)

// controlDevice answers the CLI out of the parts the daemon is already
// running. It owns no state and starts nothing: every command here is the
// same call the dashboard's own handler makes.
//
// That is the point of the type. `spoloh play` must not be a privileged
// bypass — it goes through Engine.PlayManual, which means the same single
// voice, the same priority queue and the same event log entry the /play
// button produces, so a real alert preempts it exactly as it preempts a
// preview from the browser. A second playback path would be a device that
// can be made deaf to an air raid from a shell.
type controlDevice struct {
	engine *engine.Engine
	lib    *library.Library
	log    *eventlog.Log
	cfg    func() *config.Config
}

func (d controlDevice) Status() control.Status {
	st := d.engine.Status()
	cfg := d.cfg()
	s := control.Status{
		Alert: st.Alert,
		Since: st.Since,
		// Region and the output are read from the configuration, not from
		// the engine: engine.Status deliberately leaves both to whoever
		// presents it (see its doc comment), and the dashboard does the
		// same.
		Region:      cfg.Alerts.Region,
		Source:      st.SourceName,
		LastUpdate:  st.LastSuccess,
		PollPaused:  st.PollPaused,
		SourceError: st.LastError,
		ClockSynced: st.ClockSynced,
		Output:      cfg.Audio.Device,
	}
	if st.PlayingSound != "" {
		// The engine reports the full path it handed the player; the
		// operator knows the sound by the name they uploaded.
		s.Playing = filepath.Base(st.PlayingSound)
	}
	return s
}

func (d controlDevice) Sounds() ([]string, error) {
	sounds, err := d.lib.List()
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(sounds))
	for _, s := range sounds {
		names = append(names, s.Name)
	}
	return names, nil
}

// Play uses the configured master volume, the same value the dashboard's
// /play handler reads, so a sound previewed from a shell is as loud as the
// same sound previewed from the browser.
func (d controlDevice) Play(sound string) error {
	// PlayManual, not PlayPreview: a person typing `spoloh play siren.wav`
	// into a shell is doing what the dashboard's manual-fire button does,
	// not what /sounds' listen button does. It dispatches at PriorityAlert
	// and so cannot be cut short by a scheduled chime.
	//
	// That it also cannot be preempted by an incoming air raid alert is
	// correct rather than a gap: a manual fire is already an announcement,
	// and a siren that is sounding does not need the automatic one to cut
	// in front of it.
	return d.engine.PlayManual(sound, d.cfg().Audio.MasterVolume)
}

func (d controlDevice) Stop() { d.engine.StopManual() }

func (d controlDevice) Log(n int) ([]control.Entry, error) {
	entries, err := d.log.Recent(n)
	if err != nil {
		return nil, err
	}
	out := make([]control.Entry, 0, len(entries))
	for _, e := range entries {
		out = append(out, control.Entry{
			At:    e.At,
			Kind:  e.Kind,
			Event: e.EventID,
			Sound: e.Sound,
			OK:    e.OK,
			Err:   e.Err,
		})
	}
	return out, nil
}

func (d controlDevice) Version() control.Version {
	return control.Version{Version: version, Arch: arch}
}

// serveControl runs the control socket, and keeps running it, for the reason
// serveDashboard exists: a subsystem that fails must not take the daemon
// down, and must not be given up on either.
//
// The failures this has to survive are all real and all recoverable without a
// restart: a read-only /run, a /run/spoloh that does not exist because the
// unit predates RuntimeDirectory=spoloh, a socket file somebody left behind,
// a second daemon that is on its way out. None of them is a reason for a
// device on a wall to stop announcing air raid alerts, and every one of them
// stops being true the moment whoever is fixing the board fixes it — without
// the operator having to know that the CLI needs a restart to come back.
//
// The cost is stated rather than hidden: while this is failing there is no
// CLI, the retry logs at Error every back-off, and the dashboard is the only
// way in.
func serveControl(ctx context.Context, path string, dev control.Device, logger *slog.Logger) {
	serveWithRetry(ctx, "control socket", func(ctx context.Context) error {
		return control.Serve(ctx, path, dev, logger)
	}, logger)
}
