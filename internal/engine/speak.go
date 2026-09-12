package engine

import (
	"sync"
	"time"

	"github.com/b-ovsepian/spoloh/internal/config"
	"github.com/b-ovsepian/spoloh/internal/player"
)

// speakSession is the engine's record of the stretch during which this device
// is making a sound: it opens when the player goes from silent to playing,
// and closes when it falls silent again. One session may hold a single chime,
// an event repeated five times, or a scheduled sound that a siren cut across.
//
// config.PhaseBefore and config.PhaseAfter bracket the session, once each,
// and deliberately not the individual announcement. That is the decision this
// file exists to make, so here is the case that forces it.
//
// The far end of a before/after pair is a switch with two positions. It is
// the operator's, and it is different at every site — one mutes the music
// feed that shares the speakers, one closes a relay, one selects an input,
// one wakes an amplifier — but it is always a thing put into one state so
// this device can be heard and put back afterwards. Per-announcement phases
// would fire a chime's after and then a siren's before in the gap between the
// chime being cut off and the siren starting: the switch flips back and
// forth, and whatever is on the other end does its thing for half a second at
// the exact moment the room needs to hear one clear thing. Session semantics
// fire neither, because the device never stopped speaking.
//
// The same reasoning answers three other questions without a special case for
// any of them. Event.Repeat queues each repetition as its own request (see
// Engine.play's comment), and a sequence is one session, so before fires once
// for the announcement rather than once per repetition. Back-to-back
// announcements that overlap are one session. And a truncated sound needs no
// rule of its own: one that is followed by another request is still the same
// session, and one that was the last thing in the queue ends it, because the
// device has fallen silent and the switch has to go back.
//
// The invariant everything below keeps: **for every before this device fires,
// exactly one after follows, naming the same trigger.** An operator writes
// the two halves as a pair, and a pair that can come out unmatched is worse
// than no feature at all — a switch left in the wrong position makes no
// noise, tells nobody, and stays that way until somebody works out why.
//
// The trigger the after names is the one that opened the session, never the
// one that joined it. That is what keeps the pair matched when a siren cuts
// across a chime: the operator's alert_start rows are then not the ones that
// close it, which is a real cost and is named in the doc comment on speak.
type speakSession struct {
	mu sync.Mutex

	// open is true between the before phase firing and the after phase
	// firing.
	open bool

	// trigger is what opened the session, and what its after phase will
	// name.
	trigger string

	// id changes on every open, so a hand-off parked on a before-phase delay
	// can tell whether the session it belongs to is still the one running.
	id uint64

	// handoffs counts sounds this engine has decided to play and has not
	// given the player yet, which only a before-phase delay produces. The
	// session cannot close while it is above zero: a player looking at an
	// empty queue has not finished speaking if the engine is still holding
	// the next sound behind a delay.
	handoffs int
}

// speak hands one announcement's requests to the player inside a speaking
// session. It is the only path by which an announcement reaches the player.
//
// Nothing is handed over and no phase webhook fires when there is nothing to
// play. An announcement that quiet hours silenced, or one whose sound is
// missing from the library, never opens a session: there is no sound to
// bracket, so nothing is switched and nothing is left switched.
// config.PhaseOnTrigger is the phase for a webhook that has to fire anyway,
// and it is where every webhook stored before phases existed still fires.
//
// A trigger arriving while the device is already speaking joins the session.
// It fires no phase webhook and waits for no delay — it is already being
// heard, and the switch is already where it needs to be. The cost of that is
// worth naming: an operator whose alert_start before-webhook does something
// the scheduled one does not — flashing a light, say — will not see it flash
// for a siren that preempted a chime. Weighed against the burst of whatever
// the switch controls in the middle of a raid, and against a pair that can
// come out unmatched, it is the cheaper of the two.
func (e *Engine) speak(trigger string, reqs []player.Request) {
	if len(reqs) == 0 {
		return
	}

	e.sess.mu.Lock()
	defer e.sess.mu.Unlock()

	if e.sess.open {
		e.handOverLocked(reqs)
		return
	}

	e.sess.open = true
	e.sess.trigger = trigger
	e.sess.id++
	id := e.sess.id

	// Fired under the lock so a before and the after that answers it can
	// never be dispatched out of order. Dispatch returns before a byte is
	// sent — see internal/webhook — so what is held here is a walk over the
	// configured webhooks and a few goroutine starts, not a network request.
	delay := e.fireWebhooks(config.PhaseBefore, trigger)
	if delay <= 0 {
		e.handOverLocked(reqs)
		return
	}

	e.sess.handoffs++
	go e.handOverAfter(delay, id, reqs)
}

// handOverLocked gives the player an announcement's requests with the session
// lock held.
//
// Holding it across the hand-over is what makes "is this device speaking?"
// and "here is the next sound" one decision. A player that ran dry while this
// announcement was being prepared reports that through playerIdle, which
// waits on this same lock and then asks the player again — and finds the
// requests it had not yet been given. Without that, a chime ending at the
// instant a siren was being resolved would close the session under the siren,
// and the siren would sound with the switch already put back.
func (e *Engine) handOverLocked(reqs []player.Request) {
	for _, r := range reqs {
		e.d.Player.Play(r)
	}
}

// handOverAfter waits out a before-phase delay and then gives the player the
// sound.
//
// On a goroutine of its own, and that is the whole safety argument for the
// delay existing at all. The announcing path — the poll loop, which has a
// watchdog to feed, and the scheduler tick beside it — is never held by it.
// Two announcements never queue behind one another's waits, because each has
// its own. And an alert arriving mid-wait does not join the queue behind this
// sound: it goes through speak, finds the session already open, and is handed
// to the player at once, with no delay of its own.
func (e *Engine) handOverAfter(delay time.Duration, id uint64, reqs []player.Request) {
	<-e.delayed(delay)

	e.sess.mu.Lock()
	defer e.sess.mu.Unlock()
	e.sess.handoffs--

	if !e.sess.open || e.sess.id != id {
		// StopManual, in practice: the operator silenced the device while
		// this wait was running. The sound is dropped rather than played
		// into a far end that has already been told the announcement is
		// over — and the session is closed here, because with the player
		// long since idle nothing else was ever going to.
		e.endIfSilentLocked()
		return
	}
	e.handOverLocked(reqs)
}

// playerIdle is what the player calls when its voice falls silent. See
// player.Player.OnIdle for exactly which endings reach it.
func (e *Engine) playerIdle() {
	e.sess.mu.Lock()
	defer e.sess.mu.Unlock()
	e.endIfSilentLocked()
}

// endIfSilentLocked closes the session, and fires the after phase, once
// nothing is left that could make a sound.
//
// It asks the player again rather than trusting the report that brought it
// here. OnIdle describes a moment that has already passed by the time it
// arrives; a request handed over since is still going to be played, and the
// session is not over. Declining costs nothing, because the player reports
// silence again when it really has run out.
//
// The after phase is fired under the lock for the same reason the before
// phase is, in speak: the pair must reach internal/webhook in the order they
// happened, and Dispatch does not block.
func (e *Engine) endIfSilentLocked() {
	if !e.sess.open || e.sess.handoffs > 0 {
		return
	}
	// Player is nil in tests that do not need one. A session that got as far
	// as opening without a player has nothing left to wait for.
	if e.d.Player != nil && e.d.Player.Pending() > 0 {
		return
	}
	e.sess.open = false
	e.fireWebhooks(config.PhaseAfter, e.sess.trigger)
}

// abandonHandoffs makes any sound still waiting out a before-phase delay stop
// being this session's business, and closes the session if the device is
// already silent.
//
// StopManual is the caller. "Stop" has to mean the device does not start
// speaking a second from now, and a parked hand-off would otherwise do
// exactly that. Changing the id is what tells the parked goroutine it has
// been abandoned; it is that goroutine, not this one, that decrements the
// count and ends the session, because until it wakes there is still something
// pending.
func (e *Engine) abandonHandoffs() {
	e.sess.mu.Lock()
	defer e.sess.mu.Unlock()
	e.sess.id++
	e.endIfSilentLocked()
}

// delayed is Deps.After, or time.After when nothing was injected. The
// before-phase delay is the only thing that uses it.
func (e *Engine) delayed(d time.Duration) <-chan time.Time {
	if e.d.After != nil {
		return e.d.After(d)
	}
	return time.After(d)
}

// fireWebhooks hands a phase and a trigger to the operator's own hardware, if
// any is configured, and reports how long the sound should wait afterwards.
// Nil-tolerant like every other optional dependency here, and non-blocking by
// contract — see Deps.Webhooks.
func (e *Engine) fireWebhooks(phase, trigger string) time.Duration {
	if e.d.Webhooks == nil {
		return 0
	}
	return e.d.Webhooks(phase, trigger)
}
