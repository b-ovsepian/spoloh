// Package engine wires the poller, the scheduler, and the player together.
package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"time"

	"github.com/spoloh/spoloh/internal/alertsource"
	"github.com/spoloh/spoloh/internal/alertstate"
	"github.com/spoloh/spoloh/internal/clock"
	"github.com/spoloh/spoloh/internal/config"
	"github.com/spoloh/spoloh/internal/eventlog"
	"github.com/spoloh/spoloh/internal/library"
	"github.com/spoloh/spoloh/internal/player"
	"github.com/spoloh/spoloh/internal/rules"
	"github.com/spoloh/spoloh/internal/scheduler"
)

type Deps struct {
	Config    func() *config.Config
	StatePath string
	Source    alertsource.Source
	Machine   *alertstate.Machine
	Player    *player.Player
	Library   *library.Library
	Log       *eventlog.Log
	Clock     clock.Clock
	Sync      clock.SyncChecker
	Logger    *slog.Logger
}

type Engine struct {
	d     Deps
	sched *scheduler.Runner
	rnd   *rand.Rand

	nextPoll time.Time
	attempt  int
}

func New(d Deps) *Engine {
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	e := &Engine{
		d:   d,
		rnd: rand.New(rand.NewSource(time.Now().UnixNano())),
	}
	e.sched = scheduler.NewRunner(d.Sync, d.Config, e.fireScheduled)
	return e
}

func (e *Engine) Scheduler() *scheduler.Runner { return e.sched }

// Run drives the device until ctx is cancelled.
func (e *Engine) Run(ctx context.Context) error {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			e.Tick(ctx)
		}
	}
}

// Tick performs one second of work: scheduled events always, a poll when due.
func (e *Engine) Tick(ctx context.Context) {
	now := e.d.Clock.Now()
	e.sched.Tick(now)

	if now.Before(e.nextPoll) {
		return
	}
	if err := e.PollOnce(ctx); err != nil {
		e.attempt++
		wait := alertsource.Backoff(e.attempt)
		var rl *alertsource.RateLimitError
		if errors.As(err, &rl) && rl.RetryAfter > wait {
			wait = rl.RetryAfter
		}
		e.nextPoll = now.Add(wait)
		e.d.Logger.Warn("alert poll failed", "err", err, "retry_in", wait, "attempt", e.attempt)
		return
	}
	e.attempt = 0
	e.nextPoll = now.Add(e.pollInterval())
}

// pollInterval jitters the configured interval by up to a second either way.
// Several devices behind one NAT share the upstream rate limit, and jitter
// stops them lining up into a burst.
func (e *Engine) pollInterval() time.Duration {
	base := e.d.Config().Alerts.PollInterval.Std()
	jitter := time.Duration(e.rnd.Int63n(int64(2*time.Second))) - time.Second
	if d := base + jitter; d >= config.MinPollInterval {
		return d
	}
	return config.MinPollInterval
}

// PollOnce fetches one snapshot and announces any transition.
func (e *Engine) PollOnce(ctx context.Context) error {
	cfg := e.d.Config()

	snap, err := e.d.Source.Fetch(ctx)
	if err != nil {
		e.record(eventlog.Entry{At: e.d.Clock.Now(), Kind: eventlog.KindSourceFailure, OK: false, Err: err.Error()})
		return err
	}

	st, ok := snap.Region(cfg.Alerts.Region)
	if !ok {
		err := fmt.Errorf("region %q is not present in the snapshot", cfg.Alerts.Region)
		e.record(eventlog.Entry{At: e.d.Clock.Now(), Kind: eventlog.KindSourceFailure, OK: false, Err: err.Error()})
		return err
	}

	now := e.d.Clock.Now()
	switch e.d.Machine.Apply(st, now) {
	case alertstate.Started:
		e.announce(config.TriggerAlertStart, eventlog.KindAlert, now)
	case alertstate.Ended:
		e.announce(config.TriggerAlertEnd, eventlog.KindAlert, now)
	default:
		return nil
	}

	// Written only on a transition: this file lives on an SD card.
	if e.d.StatePath != "" {
		if err := alertstate.SavePersisted(e.d.StatePath, e.d.Machine.Persisted()); err != nil {
			e.d.Logger.Warn("could not persist alert state", "err", err)
		}
	}
	return nil
}

// PlayManual plays a library sound on the operator's command. It deliberately
// bypasses quiet hours: someone pressed the button on purpose, and this is the
// path that still works when upstream is down but the threat is real.
func (e *Engine) PlayManual(sound string, volume int) error {
	path := e.d.Library.Path(sound)
	if path == "" || !e.d.Library.Has(sound) {
		return fmt.Errorf("sound %q is not in the library", sound)
	}
	e.d.Player.Play(player.Request{
		EventID:  "manual",
		Path:     path,
		Volume:   volume,
		Priority: player.PriorityAlert,
	})
	e.record(eventlog.Entry{At: e.d.Clock.Now(), Kind: eventlog.KindManual, Sound: sound, OK: true})
	return nil
}

func (e *Engine) announce(trigger, kind string, at time.Time) {
	for _, d := range rules.Resolve(e.d.Config(), trigger, at) {
		e.play(d, kind, at)
	}
}

func (e *Engine) fireScheduled(ev config.Event, at time.Time) {
	d, ok := rules.ForEvent(e.d.Config(), ev, at)
	if !ok {
		return
	}
	e.play(d, eventlog.KindScheduled, at)
}

func (e *Engine) play(d rules.Decision, kind string, at time.Time) {
	path := e.d.Library.Path(d.Sound)
	if path == "" || !e.d.Library.Has(d.Sound) {
		e.d.Logger.Error("sound is missing from the library", "sound", d.Sound, "event", d.EventID)
		e.record(eventlog.Entry{
			At: at, Kind: eventlog.KindPlaybackError, EventID: d.EventID,
			Sound: d.Sound, OK: false, Err: "sound not found in library",
		})
		return
	}
	e.d.Player.Play(player.Request{
		EventID:  d.EventID,
		Path:     path,
		Volume:   d.Volume,
		Priority: d.Priority,
	})
	e.d.Logger.Info("announcing", "kind", kind, "event", d.EventID, "sound", d.Sound, "volume", d.Volume)
	e.record(eventlog.Entry{At: at, Kind: kind, EventID: d.EventID, Sound: d.Sound, OK: true})
}

func (e *Engine) record(entry eventlog.Entry) {
	if e.d.Log == nil {
		return
	}
	if err := e.d.Log.Append(entry); err != nil {
		e.d.Logger.Warn("could not write the event log", "err", err)
	}
}
