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
	"github.com/spoloh/spoloh/internal/watchdog"
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

	// Watchdog, when non-nil, is pinged at the end of every completed tick.
	// Pinging from the loop rather than from a timer is the point: a stalled
	// loop stops pinging and systemd restarts the daemon.
	Watchdog *watchdog.Notifier
}

// heartbeatInterval bounds how often the liveness line is written. Without
// it, a daemon that is wedged — no transitions, no errors, just silently not
// polling — looks byte-identical in the journal to a healthy one; logging
// every poll at the default 5s cadence, on the other hand, would write
// roughly 17,000 lines a day to an SD card and bury the handful that matter.
const heartbeatInterval = 15 * time.Minute

// fetchDeadline caps one poll's entire source cascade, however many sources
// the operator has configured.
//
// The arithmetic it has to respect: deploy/spoloh.service sets WatchdogSec=60,
// and systemd wants a ping at least twice per period, so watchdog.Interval
// throttles pings to one every 30s. alertsource.Client gives each request its
// own 10s timeout and tries the configured sources one after another, so with
// the three default sources a single failing poll blocked the tick for 30s,
// putting the maximum gap between pings at exactly 30+30 = 60s — precisely on
// the deadline. With all six sources the spec lists, a failing poll would kill
// the daemon every time.
//
// 20s keeps a whole cascade comfortably below the 30s ping interval whatever
// the source count, while still allowing two full 10s attempts before the
// backoff takes over.
const fetchDeadline = 20 * time.Second

type Engine struct {
	d     Deps
	sched *scheduler.Runner
	rnd   *rand.Rand

	nextPoll time.Time
	pollWait time.Duration // the wait Tick most recently scheduled nextPoll for
	attempt  int

	firstPollLogged bool
	failing         bool
	lastSuccess     time.Time
	nextHeartbeat   time.Time
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
	e.sched.Logger = d.Logger
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
	defer e.heartbeat(now)

	e.sched.Tick(now)

	// A clock that steps backwards must not leave the poll gate shut for the
	// whole of the step. clock.System.Now() calls time.Time.In, which strips
	// the monotonic reading, so an NTP correction on a board with no RTC is a
	// pure backwards jump in the values compared here — nothing crashes, the
	// ticker keeps firing, and without this guard the gate would stay shut
	// for the entire step with nothing in the journal to show it. The
	// remaining wait can never legitimately exceed the wait we actually
	// scheduled, so anything larger means the clock moved, not that we are
	// early; that needs no arbitrary threshold to detect.
	if e.nextPoll.Sub(now) > e.pollWait {
		e.nextPoll = now
	}

	if now.Before(e.nextPoll) {
		e.d.Watchdog.Alive()
		return
	}
	// Ping before the blocking poll as well as after it. A poll can hold the
	// tick for up to fetchDeadline, and a ping only on the far side of that
	// would let one slow cascade eat the whole watchdog budget.
	e.d.Watchdog.Alive()
	if err := e.PollOnce(ctx); err != nil {
		e.attempt++
		wait := alertsource.Backoff(e.attempt)
		var rl *alertsource.RateLimitError
		if errors.As(err, &rl) && rl.RetryAfter > wait {
			wait = rl.RetryAfter
		}
		e.nextPoll = now.Add(wait)
		e.pollWait = wait
		e.d.Logger.Warn("alert poll failed", "err", err, "retry_in", wait, "attempt", e.attempt)
		// A failed poll still counts as progress: upstream being down is not
		// the daemon being stuck, and restarting on it would turn an outage
		// into a reboot loop.
		e.d.Watchdog.Alive()
		return
	}
	e.attempt = 0
	wait := e.pollInterval()
	e.nextPoll = now.Add(wait)
	e.pollWait = wait
	e.d.Watchdog.Alive()
}

// heartbeat writes a low-rate liveness line: the time of the last successful
// poll and the current alert state. It uses the same backwards-clock guard as
// the poll gate, for the same reason.
func (e *Engine) heartbeat(now time.Time) {
	if e.nextHeartbeat.Sub(now) > heartbeatInterval {
		e.nextHeartbeat = now
	}
	if now.Before(e.nextHeartbeat) {
		return
	}
	e.d.Logger.Info("heartbeat", "last_success", e.lastSuccess, "alert", e.d.Machine.Alert())
	e.nextHeartbeat = now.Add(heartbeatInterval)
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

	// One deadline over the whole cascade, not one per source: see
	// fetchDeadline for the watchdog arithmetic it protects.
	fetchCtx, cancel := context.WithTimeout(ctx, fetchDeadline)
	defer cancel()

	snap, err := e.d.Source.Fetch(fetchCtx)
	if err != nil {
		e.failing = true
		e.record(eventlog.Entry{At: e.d.Clock.Now(), Kind: eventlog.KindSourceFailure, OK: false, Err: err.Error()})
		return err
	}

	st, ok := snap.Region(cfg.Alerts.Region)
	if !ok {
		err := fmt.Errorf("region %q is not present in the snapshot", cfg.Alerts.Region)
		e.failing = true
		e.record(eventlog.Entry{At: e.d.Clock.Now(), Kind: eventlog.KindSourceFailure, OK: false, Err: err.Error()})
		return err
	}

	now := e.d.Clock.Now()
	e.lastSuccess = now
	e.reportHealth(cfg, snap, st)

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

// reportHealth logs only the poll's reachability transitions: the first
// successful poll since the process started, and recovery from a run of
// failures. Every other successful poll is silent by design — see
// heartbeatInterval's comment for why logging all of them would be worse
// than logging none.
func (e *Engine) reportHealth(cfg *config.Config, snap alertsource.Snapshot, st alertsource.RegionState) {
	switch {
	case !e.firstPollLogged:
		e.d.Logger.Info("alert source reachable", "source", snap.Source, "region", cfg.Alerts.Region, "alert", st.Alert)
		e.firstPollLogged = true
	case e.failing:
		e.d.Logger.Info("alert source recovered", "source", snap.Source, "region", cfg.Alerts.Region, "alert", st.Alert)
	}
	e.failing = false
}

func (e *Engine) record(entry eventlog.Entry) {
	if e.d.Log == nil {
		return
	}
	if err := e.d.Log.Append(entry); err != nil {
		e.d.Logger.Warn("could not write the event log", "err", err)
	}
}
