// Package engine wires the poller, the scheduler, and the player together.
package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"path/filepath"
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

	// Webhooks, when non-nil, is called with the trigger of an
	// announcement this device is making, so an operator's own hardware —
	// a relay board, a PA controller — acts along with the speaker. It is
	// filled by internal/webhook.Dispatcher.Dispatch, and it must return
	// without waiting on a network: it is called a few statements before
	// the sound is queued, and the siren does not wait for a box that may
	// be unplugged.
	Webhooks func(trigger string)

	// Watchdog, when non-nil, is pinged at the end of every completed tick.
	// Pinging from the loop rather than from a timer is the point: a stalled
	// loop stops pinging and systemd restarts the daemon.
	Watchdog *watchdog.Notifier
}

// heartbeatInterval bounds how often the liveness line is written. Without
// it, a daemon that is wedged — no transitions, no errors, just silently not
// polling — looks byte-identical in the journal to a healthy one; logging
// every poll at the default 15s cadence, on the other hand, would write
// roughly 5,700 lines a day to an SD card and bury the handful that matter.
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

	// allClearSeen mirrors alertstate.Machine.CalmRun for the dashboard, on
	// the same terms as alert and alertSince above: the machine is
	// single-goroutine by contract, so the value is copied here at the
	// moment it changes rather than read across the boundary.
	allClearSeen int

	// level mirrors alertstate.Machine.Level, on the same terms again. It
	// is on the dashboard because the two words mean different things to
	// whoever is reading the screen — a yellow warning and a red air raid
	// ask different things of them — and because a device that announced an
	// escalation must be able to show, afterwards, which of the two it is
	// currently saying.
	level alertsource.Level

	// sources is what the last poll's cascade did, one entry per configured
	// key. Written by fetch on every poll, successful or not — a cascade
	// that failed everywhere is exactly the poll an operator most needs the
	// dashboard to describe.
	sources []SourceHealth
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
		e.level = d.Machine.Level()
	}
	// The player is the only thing that sees a sound cut short, and the
	// engine is where every other row about an announcement is written, so
	// the correction is wired next to the claim it corrects rather than in
	// cmd/spoloh where the two could drift apart. Set here because Player
	// wants its callbacks in place before Run, which the daemon starts long
	// after this.
	if d.Player != nil {
		d.Player.OnTruncated = e.recordTruncated
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

	snap, err := e.fetch(fetchCtx)
	if err != nil {
		e.pollFailed(err)
		return err
	}

	st, watched, ok := resolveWatched(snap, cfg)
	if !ok {
		err := fmt.Errorf("region %q is not present in the snapshot", cfg.Alerts.Region)
		e.pollFailed(err)
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
	// Keyed on what was actually read, not on the oblast alone: an operator
	// narrowing from «Сумська область» to «м. Шостка та Шосткинська
	// територіальна громада» leaves the oblast field untouched, so keying on
	// it would compare the hromada's first reading against the oblast's last
	// one and announce the difference — a siren, or an all-clear, for a
	// transition this device witnessed nowhere. That is the same rule spec
	// §5.2 forbids breaking, arriving by a second route.
	if watched != e.pollRegion {
		if e.pollRegion != "" {
			e.d.Logger.Info("configured oblast changed; adopting its current state without announcing",
				"from", e.pollRegion, "to", watched, "alert", st.Alert)
			e.d.Machine.Rebaseline()
			rebaselined = true
		}
		e.pollRegion = watched
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
	e.allClearSeen = e.d.Machine.CalmRun()
	e.level = e.d.Machine.Level()
	e.mu.Unlock()

	// An all-clear being held back is the state an operator debugging "the
	// raid is over, why is this thing still red" lands in, and the journal
	// is where they look when the screen is not in front of them. At most
	// EndConfirmations-1 lines per raid, so it is an event, not a cadence.
	if seen := e.d.Machine.CalmRun(); seen > 0 {
		e.d.Logger.Info("upstream reports calm; holding the all-clear until it is confirmed",
			"seen", seen, "need", alertstate.EndConfirmations)
	}

	switch transition {
	case alertstate.Started:
		e.announce(config.TriggerAlertStart, eventlog.KindAlert, now)
	case alertstate.Ended:
		e.announce(config.TriggerAlertEnd, eventlog.KindAlert, now)
	case alertstate.Escalated:
		e.d.Logger.Warn("the raid has been declared red", "region", watched, "was", "yellow")
		e.announceEscalation(now)
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

// fetch asks the source for one snapshot and records what the cascade did on
// the way to it.
//
// The reporting is a type assertion rather than a wider Deps.Source, because
// which source answered is a thing the dashboard wants and the poll loop does
// not: every decision below this line — the transition, the announcement, the
// state file — is made from the snapshot alone and would be identical without
// a single Attempt. A source that cannot report a cascade still polls
// normally and simply describes nothing, which is what Status.Sources being
// nil means.
//
// The attempts are mirrored whether or not the fetch succeeded. A cascade
// where everything failed is the poll the card exists for, and dropping it on
// the error path would leave the dashboard showing the last cascade that
// worked while the device hears nothing.
func (e *Engine) fetch(ctx context.Context) (alertsource.Snapshot, error) {
	cascade, ok := e.d.Source.(alertsource.Cascade)
	if !ok {
		return e.d.Source.Fetch(ctx)
	}
	snap, attempts, err := cascade.FetchCascade(ctx)

	health := make([]SourceHealth, len(attempts))
	for i, a := range attempts {
		// Reason(), never Err: see SourceHealth. This is the one place the
		// two are separated, so there is nothing downstream that could
		// choose wrongly.
		health[i] = SourceHealth{Key: a.Key, Outcome: a.Outcome, Reason: a.Reason()}
	}
	e.mu.Lock()
	e.sources = health
	e.mu.Unlock()

	return snap, err
}

// pollFailed records a poll that produced no usable reading, whether the
// whole source cascade failed or the snapshot did not carry the configured
// oblast. The two used to be written out separately and had already drifted
// by one statement.
//
// It also tells the machine, which is not bookkeeping: a run of calm readings
// only counts as confirmation of an all-clear while it is unbroken, and a
// poll that saw nothing is exactly the break. See
// alertstate.Machine.PollFailed.
func (e *Engine) pollFailed(err error) {
	e.d.Machine.PollFailed()
	e.mu.Lock()
	e.failing = true
	e.lastError = err.Error()
	e.allClearSeen = 0
	e.mu.Unlock()
	e.record(eventlog.Entry{At: e.d.Clock.Now(), Kind: eventlog.KindSourceFailure, OK: false, Err: err.Error()})
}

// PlayManual plays a library sound on the operator's command. It deliberately
// bypasses quiet hours: someone pressed the button on purpose, and this is the
// path that still works when upstream is down but the threat is real.
func (e *Engine) PlayManual(sound string, volume int) error {
	path := e.d.Library.Path(sound)
	if path == "" || !e.d.Library.Has(sound) {
		return fmt.Errorf("sound %q is not in the library", sound)
	}
	return e.playManual(sound, path, volume, player.PriorityAlert)
}

// PlayPreview plays a library sound to find out what it is or whether the
// speaker works — the /sounds preview, the /audio test, `spoloh play`.
//
// Identical to PlayManual except that it cannot delay anything real. Both
// used to be PlayManual, which dispatches at PriorityAlert so that a manual
// fire is not cut short by a scheduled chime; a preview inheriting that
// could hold the single voice for up to five minutes while an actual air
// raid alert queued behind it.
//
// Still the same engine, the same queue and the same output — a preview
// that played through a different path would not be previewing the thing it
// claims to.
func (e *Engine) PlayPreview(sound string, volume int) error {
	path := e.d.Library.Path(sound)
	if path == "" || !e.d.Library.Has(sound) {
		return fmt.Errorf("sound %q is not in the library", sound)
	}
	return e.playManual(sound, path, volume, player.PriorityPreview)
}

func (e *Engine) playManual(sound, path string, volume int, priority player.Priority) error {
	e.d.Player.Play(player.Request{
		EventID:  "manual",
		Path:     path,
		Volume:   volume,
		Priority: priority,
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
	// Fired on the transition itself, before the event list or quiet hours
	// get a say in whether anything is played — and deliberately so for
	// TriggerAlertEnd. A webhook usually drives something that latches: the
	// relay the operator asked for closes when the raid starts. If the
	// end-of-alert webhook fired only when the all-clear was announced,
	// then an alert-end event switched off, or a quiet-hours window
	// covering alerts, would leave that relay closed after the raid was
	// over, with nothing left to open it. Quiet hours are about what the
	// device makes people hear at night; they are not an instruction to
	// leave somebody else's hardware latched on.
	//
	// This is the transition, not a poll: announce is reached only from
	// alertstate.Started and alertstate.Ended, and Ended already waits for
	// EndConfirmations consecutive calm readings. A webhook therefore never
	// says the raid is over on the strength of one source blinking.
	e.fireWebhooks(trigger)
	e.playTrigger(trigger, kind, at)
}

// announceEscalation announces a raid that was declared yellow and has been
// declared red.
//
// The webhook always fires on TriggerAlertEscalated. It is a name no
// configuration written before this build can carry, so an operator who has
// one meant it, and firing the start's webhook instead would close a relay
// that is already closed while leaving theirs alone.
//
// The sound falls back to TriggerAlertStart when no enabled event names the
// escalation, which is every configuration in the field today. An escalation
// answered with silence because nobody had opened a form yet would be this
// device gaining a level and losing its siren, and a rise in severity is a
// start's kind of news: people who heard the lesser warning need to be told
// the threat has grown, with the same sound that told them it existed.
func (e *Engine) announceEscalation(at time.Time) {
	e.fireWebhooks(config.TriggerAlertEscalated)

	trigger := config.TriggerAlertEscalated
	if !configured(e.d.Config(), trigger) {
		trigger = config.TriggerAlertStart
	}
	e.playTrigger(trigger, eventlog.KindAlert, at)
}

// configured reports whether the operator has an enabled event on this
// trigger.
//
// It reads the event list rather than asking whether rules.Resolve returns
// anything, because "nothing is configured for this" and "quiet hours
// silenced it" must not be the same answer. Conflating them would let a
// silenced escalation fall back to the start trigger, be silenced again, and
// write the operator two suppression rows for one raid.
func configured(cfg *config.Config, trigger string) bool {
	for _, ev := range cfg.Events {
		if ev.Trigger == trigger && ev.Enabled {
			return true
		}
	}
	return false
}

// playTrigger is the half of an announcement that reaches the speaker: what
// quiet hours silenced, written down, and what survives, played.
func (e *Engine) playTrigger(trigger, kind string, at time.Time) {
	cfg := e.d.Config()
	// Quiet hours can be configured to cover alerts, and then this is a
	// trigger that produces no decisions, no playback and — without the
	// loop below — no trace of any kind. That is the one outcome this
	// device must never leave behind: an operator asking the next morning
	// why nothing sounded at 03:40 has the event log and nothing else.
	// rules.Suppressions reports alerts only; a scheduled announcement
	// skipped at night is the setting working, and logging those would bury
	// the rows that matter.
	for _, s := range rules.Suppressions(cfg, trigger, at) {
		e.d.Logger.Warn("quiet hours silenced an alert",
			"event", s.EventID, "sound", s.Sound,
			"from", cfg.QuietHours.From, "to", cfg.QuietHours.To)
		e.record(eventlog.Entry{
			At: at, Kind: eventlog.KindAlertSuppressed, EventID: s.EventID, Sound: s.Sound,
			// OK=false and an operator-facing reason, in Ukrainian like the
			// rest of what /log shows: the row has to say which setting did
			// this, or it only moves the question along.
			OK:  false,
			Err: fmt.Sprintf("тихі години %s–%s", cfg.QuietHours.From, cfg.QuietHours.To),
		})
	}
	for _, d := range rules.Resolve(cfg, trigger, at) {
		e.play(d, kind, at)
	}
}

func (e *Engine) fireScheduled(ev config.Event, at time.Time) {
	d, ok := rules.ForEvent(e.d.Config(), ev, at)
	if !ok {
		return
	}
	// After the decision, unlike announce above: a scheduled announcement
	// latches nothing, so a webhook for one that quiet hours silenced would
	// be the device acting at a moment when it deliberately did not. The
	// two rules differ because what they protect differs, not by oversight.
	e.fireWebhooks(config.TriggerScheduled)
	e.play(d, eventlog.KindScheduled, at)
}

// fireWebhooks hands a trigger to the operator's own hardware, if any is
// configured. Nil-tolerant like every other optional dependency here, and
// non-blocking by contract — see Deps.Webhooks.
func (e *Engine) fireWebhooks(trigger string) {
	if e.d.Webhooks != nil {
		e.d.Webhooks(trigger)
	}
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

// recordTruncated writes the row that says a sound this device queued did not
// finish. It is installed on the player by New and called from the player's
// goroutine; eventlog.Log is safe for concurrent Append and e.record tolerates
// a nil Log, so there is nothing here to serialise.
//
// play() writes its row the moment a request is queued, which is what lets a
// power cut mid-siren still leave evidence that the siren was sounded — and
// which also means that row cannot know how the sound ended. This is the
// other half of the pair: without it the log says the minute of silence
// played, to the one person reading it to find out why nobody heard it.
func (e *Engine) recordTruncated(r player.Request, reason string) {
	// The operator-facing wording is written here rather than in the player,
	// which deals in English identifiers like OnError's stage names. Anything
	// unrecognised still produces a row: a reason nobody has a phrase for is
	// far better shown than swallowed.
	why := reason
	switch reason {
	case player.CutPreempted:
		why = "перервано важливішим сигналом"
	case player.CutStopped:
		why = "зупинено оператором"
	}
	sound := filepath.Base(r.Path)
	e.d.Logger.Info("a sound was cut short", "event", r.EventID, "sound", sound, "reason", reason)
	e.record(eventlog.Entry{
		At: e.d.Clock.Now(), Kind: eventlog.KindPlaybackTruncated,
		EventID: r.EventID, Sound: sound, OK: false, Err: why,
	})
}

func (e *Engine) record(entry eventlog.Entry) {
	if e.d.Log == nil {
		return
	}
	if err := e.d.Log.Append(entry); err != nil {
		e.d.Logger.Warn("could not write the event log", "err", err)
	}
}

// resolveWatched picks the state this poll is about, and names what it is.
//
// A configured LocationUID is tried first: a provider that understands
// hromadas answers for exactly the place the operator chose. When the
// snapshot holds no such UID — every Ubilling source, which knows oblasts and
// nothing else — this broadens to the configured oblast rather than failing.
//
// Broadening is the documented contract (see alertsource.OblastOf) and it is
// the safe direction: a fallback announces for the whole oblast, which is
// more than was asked for and never less. Failing instead would take the
// device silent over a source that is answering perfectly well, one level
// coarser than asked — and silence is the one outcome this project refuses.
//
// The second return is what was actually read, which the re-baseline check
// keys on. It has to be the place, not the oblast: narrowing from an oblast
// to a hromada inside it changes what the boolean means without changing
// cfg.Alerts.Region at all.
func resolveWatched(snap alertsource.Snapshot, cfg *config.Config) (alertsource.RegionState, string, bool) {
	if uid := cfg.Alerts.LocationUID; uid != 0 {
		if st, ok := snap.Location(uid); ok {
			return st, st.Region, true
		}
	}
	st, ok := snap.Region(cfg.Alerts.Region)
	return st, cfg.Alerts.Region, ok
}
