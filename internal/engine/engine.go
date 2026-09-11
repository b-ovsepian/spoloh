// Package engine wires the poller, the scheduler, and the player together.
package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"sync"
	"time"

	"github.com/b-ovsepian/spoloh/internal/alertsource"
	"github.com/b-ovsepian/spoloh/internal/alertstate"
	"github.com/b-ovsepian/spoloh/internal/clock"
	"github.com/b-ovsepian/spoloh/internal/config"
	"github.com/b-ovsepian/spoloh/internal/eventlog"
	"github.com/b-ovsepian/spoloh/internal/library"
	"github.com/b-ovsepian/spoloh/internal/player"
	"github.com/b-ovsepian/spoloh/internal/rules"
	"github.com/b-ovsepian/spoloh/internal/scheduler"
	"github.com/b-ovsepian/spoloh/internal/watchdog"
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

	nextHeartbeat time.Time

	// pollRegion is the oblast the last poll actually read, so a region
	// changed under the engine at runtime can be noticed. It is written and
	// read only in PollOnce, which the poll loop calls from its own single
	// goroutine, like nextPoll and attempt above.
	pollRegion string

	// mu guards the fields below, which the dashboard reads from an HTTP
	// goroutine while the engine writes them from its own. alertstate.Machine
	// is single-goroutine by contract, so the two values the dashboard needs
	// from it are mirrored here at the moment they change rather than read
	// across the boundary.
	//
	// firstPollLogged is guarded by mu too, like its neighbours below, even
	// though it is not itself exposed on Status: it is read-then-written by
	// the same reportHealth call that reads/writes failing, and living
	// outside the lock would be an easy thing to miss the next time someone
	// reaches for it.
	mu              sync.RWMutex
	firstPollLogged bool
	alert           bool
	alertSince      time.Time
	failing         bool
	lastError       string
	lastSuccess     time.Time
	sourceName      string
}

func New(d Deps) *Engine {
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	e := &Engine{
		d:   d,
		rnd: rand.New(rand.NewSource(time.Now().UnixNano())),
	}
	// Seed the dashboard's mirrored alert fields from the machine, which
	// cmd/spoloh has already Restore'd from state.json by the time it gets
	// here. Without this they stay false until the first successful poll
	// writes them, so a device that lost power mid-alert would come back up
	// showing a large calm state — indefinitely if upstream is unreachable,
	// which is precisely the situation this dashboard exists to serve. Doing
	// it in New rather than at the top of Run keeps it ahead of any goroutine
	// that could read Status, so there is nothing to race with.
	//
	// This mirrors, it does not decide: Machine.Restore deliberately leaves
	// the machine un-baselined, so the first Apply still cannot replay a
	// siren for an alert that was already running (spec §5.2).
	if d.Machine != nil {
		e.alert = d.Machine.Alert()
		e.alertSince = d.Machine.Since()
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
	// Before the first successful poll there is no timestamp to report, and
	// printing the zero time reads as a clock fault rather than as a device
	// that has simply not got an answer yet.
	e.mu.RLock()
	last := e.lastSuccess
	e.mu.RUnlock()
	lastSuccess := "none yet"
	if !last.IsZero() {
		lastSuccess = last.Format(time.RFC3339)
	}
	e.d.Logger.Info("heartbeat", "last_success", lastSuccess, "alert", e.d.Machine.Alert())
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
		e.mu.Lock()
		e.failing = true
		e.lastError = err.Error()
		e.mu.Unlock()
		e.record(eventlog.Entry{At: e.d.Clock.Now(), Kind: eventlog.KindSourceFailure, OK: false, Err: err.Error()})
		return err
	}

	st, ok := snap.Region(cfg.Alerts.Region)
	if !ok {
		err := fmt.Errorf("region %q is not present in the snapshot", cfg.Alerts.Region)
		e.mu.Lock()
		e.failing = true
		e.lastError = err.Error()
		e.mu.Unlock()
		e.record(eventlog.Entry{At: e.d.Clock.Now(), Kind: eventlog.KindSourceFailure, OK: false, Err: err.Error()})
		return err
	}

	now := e.d.Clock.Now()
	e.reportHealth(cfg, snap, st)

	// The oblast can change while the daemon runs: /network writes
	// Alerts.Region, and this reads it fresh every poll. alertstate.Machine
	// tracks a bare boolean and has no idea the boolean now comes from a
	// different oblast, so without this the new oblast's first reading is
	// compared against the old oblast's last one — switch a device from an
	// oblast under alert to a calm one and it announces an all-clear over
	// the site's speakers for an alert nobody in that building ever heard;
	// switch it the other way and it plays the full siren. Re-baselining
	// makes the new oblast's first reading silent, exactly as a cold start
	// is, which is what spec §5.2 requires: never announce a transition the
	// device did not witness.
	//
	// The first poll of a process is not a change and must not re-baseline:
	// the machine has just been Restore'd from disk, and that is the one
	// piece of evidence that lets Apply announce a transition that genuinely
	// happened while the device was down.
	rebaselined := false
	if cfg.Alerts.Region != e.pollRegion {
		if e.pollRegion != "" {
			e.d.Logger.Info("configured oblast changed; adopting its current state without announcing",
				"from", e.pollRegion, "to", cfg.Alerts.Region, "alert", st.Alert)
			e.d.Machine.Rebaseline()
			rebaselined = true
		}
		e.pollRegion = cfg.Alerts.Region
	}

	transition := e.d.Machine.Apply(st, now)

	// Mirrored for the dashboard on every successful poll, including one
	// that produces no transition — a quiet day must still read as a
	// healthy device, not a frozen one.
	e.mu.Lock()
	e.failing = false
	e.lastError = ""
	e.lastSuccess = now
	e.sourceName = snap.Source
	e.alert = e.d.Machine.Alert()
	e.alertSince = e.d.Machine.Since()
	e.mu.Unlock()

	switch transition {
	case alertstate.Started:
		e.announce(config.TriggerAlertStart, eventlog.KindAlert, now)
	case alertstate.Ended:
		e.announce(config.TriggerAlertEnd, eventlog.KindAlert, now)
	default:
		// A re-baseline writes too, even though it announced nothing. The
		// file now names the oblast its reading came from, and until the new
		// pair is on disk the stored state still names the old oblast — which
		// the next boot would correctly discard, silently losing the
		// missed-transition rule for as long as the new oblast stays calm.
		// One write per oblast change is not a cadence, it is an event.
		if !rebaselined {
			return nil
		}
	}

	// Written only on a transition or a re-baseline: this file lives on an
	// SD card.
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

// StopManual silences whatever is on the speaker, on the operator's command,
// and records that it happened.
//
// /play goes through PlayManual and leaves a KindManual entry behind; /stop
// used to call Player.Stop directly and leave nothing, so the site's log
// showed that someone started the siren and never that they stopped it —
// and stopping is the one dashboard action that can make this device go
// quiet. Player.Stop cancels the in-flight request as well as the queue,
// which is what the button in a hurry has to mean.
func (e *Engine) StopManual() {
	// Player is nil in tests that do not need one; there is nothing to stop,
	// which is a no-op rather than an error, but the operator's action is
	// still worth recording.
	if e.d.Player != nil {
		e.d.Player.Stop()
	}
	e.record(eventlog.Entry{At: e.d.Clock.Now(), Kind: eventlog.KindManual, EventID: "stop", OK: true})
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
	// d.Repeat is one Player.Play call repeated, not a single request the
	// player somehow plays more than once: each repetition is its own
	// queued Request, so an alert arriving mid-sequence preempts it exactly
	// as it would preempt any other single scheduled sound (see
	// player.Player.Play's priority ordering). rules.ForEvent already floors
	// Repeat at 1, but the same floor is repeated here rather than trusted
	// blindly, so a Decision built by anything other than ForEvent cannot
	// silently skip the announcement.
	repeat := d.Repeat
	if repeat < 1 {
		repeat = 1
	}
	for i := 0; i < repeat; i++ {
		e.d.Player.Play(player.Request{
			EventID:  d.EventID,
			Path:     path,
			Volume:   d.Volume,
			Priority: d.Priority,
		})
	}
	e.d.Logger.Info("announcing", "kind", kind, "event", d.EventID, "sound", d.Sound, "volume", d.Volume, "repeat", repeat)
	e.record(eventlog.Entry{At: at, Kind: kind, EventID: d.EventID, Sound: d.Sound, OK: true})
}

// reportHealth logs only the poll's reachability transitions: the first
// successful poll since the process started, and recovery from a run of
// failures. Every other successful poll is silent by design — see
// heartbeatInterval's comment for why logging all of them would be worse
// than logging none.
func (e *Engine) reportHealth(cfg *config.Config, snap alertsource.Snapshot, st alertsource.RegionState) {
	// All the state this decides on and updates is guarded by mu, so it is
	// read and written under one lock and released before logging — logging
	// while holding the lock would be a worse habit than the one this task
	// exists to fix.
	e.mu.Lock()
	wasFailing := e.failing
	firstPoll := !e.firstPollLogged
	if firstPoll {
		e.firstPollLogged = true
	}
	e.mu.Unlock()

	switch {
	case firstPoll:
		e.d.Logger.Info("alert source reachable", "source", snap.Source, "region", cfg.Alerts.Region, "alert", st.Alert)
	case wasFailing:
		e.d.Logger.Info("alert source recovered", "source", snap.Source, "region", cfg.Alerts.Region, "alert", st.Alert)
	}
	// The reset of failing to false is recorded once, uniformly, in
	// PollOnce's mirror block right after this returns — not here, so there
	// is a single place that clears a failure on success.
}

func (e *Engine) record(entry eventlog.Entry) {
	if e.d.Log == nil {
		return
	}
	if err := e.d.Log.Append(entry); err != nil {
		e.d.Logger.Warn("could not write the event log", "err", err)
	}
}
