package netmgr

import (
	"context"
	"log/slog"
	"time"

	"github.com/b-ovsepian/spoloh/internal/clock"
	"github.com/b-ovsepian/spoloh/internal/config"
)

// tickInterval is how often the supervisor looks at the network. Every tick
// runs one nmcli command, so this is deliberately slower than the engine's:
// nothing here needs second-level reaction, and the device is a Pi.
const tickInterval = 5 * time.Second

// failureLogInterval throttles repeated failure logging for the status read
// and for StartHotspot and StopHotspot. The work itself stays unconditional:
// a device stuck unable to raise or lower its access point must keep trying
// every tick, or it either stays unreachable or keeps its own AP up over a
// working network forever.
//
// Only the logging is throttled. At tickInterval an unthrottled line would be
// written every 5 seconds indefinitely, and the status read in particular
// fails on every tick, forever, on any board whose OS simply has no
// NetworkManager installed. internal/engine's heartbeatInterval throttles its
// own 5s-cadence tick the same way for the same reason: these devices run off
// SD cards that wear out (spec §5.7).
const failureLogInterval = 5 * time.Minute

// tickDeadline caps the nmcli work of one tick. NetworkManager can block for
// a long time or forever — a wedged wpa_supplicant, a scan that never comes
// back, an activation waiting on a DHCP lease — and Tick runs synchronously in
// Run's only goroutine. Without a bound, one hung command stops the supervisor
// permanently, and a device that stops supervising itself never raises the
// access point that is its last way back. internal/engine bounds its poll the
// same way and for the same reason.
//
// It is deliberately much larger than tickInterval: raising an access point
// legitimately takes several seconds, and Run's ticker drops ticks it cannot
// deliver, so a slow tick costs cadence rather than correctness.
const tickDeadline = 30 * time.Second

// searchInterval is how long the access point stays up, with nothing above
// it, before the supervisor lowers it to let NetworkManager look for a
// network it remembers. On a single-radio board the access point is the
// radio, and NetworkManager only autoconnects a device that is free: without
// this, a device whose router was simply slower to boot after a power cut
// serves its own access point forever and receives no alerts.
//
// A board with a second radio (a USB dongle beside the onboard Wi-Fi) loses
// its access point for a search it did not need. That is accepted: telling
// the two apart is more machinery than one search window every few minutes
// costs.
const searchInterval = 5 * time.Minute

// searchWindow is how long the radio is left to NetworkManager once the
// access point is down. It is a constant and not Network.APFallbackAfter,
// which the boot grace uses, for two reasons. NetworkManager's own DHCP
// timeout is 45 seconds and only starts after a scan and an association on a
// radio that was serving an access point a moment ago, so a window that short
// can end mid-lease and raise the access point over a join about to succeed —
// every cycle. And a hand-edited ap_fallback_after of an hour would otherwise
// take the access point down for an hour at a time.
const searchWindow = 90 * time.Second

// clockStep is how far apart two looks at the access point can be before the
// gap is taken for the clock moving rather than time passing. Ticks come
// every tickInterval, and a slow one costs at most tickDeadline, so anything
// past a minute is a step. See searchForSavedNetwork.
const clockStep = time.Minute

// searchQuietFor is how recently a dashboard request must have arrived for
// the search to wait: someone on the setup screen is using the access point,
// and pulling it from under them every five minutes would make the device
// impossible to configure. See Activity for why requests and not clients.
const searchQuietFor = 2 * time.Minute

// searchCeiling bounds that wait. A tab left open on the setup screen, or an
// operator who wanders off mid-page, must not keep the device off the network
// it remembers indefinitely; at the worst the operator loses the page for one
// search window and reconnects.
const searchCeiling = 15 * time.Minute

// Supervisor keeps the device reachable. It holds no belief about whether the
// hotspot is up — it reads that from the manager each tick, because a belief
// the world can contradict is a bug waiting to happen.
//
// Tick and Run are for a single goroutine: downSince, the search state and
// the failure-log timestamps below are read and written without
// synchronization. That is safe because the object the rest of the daemon
// shares across goroutines is the Manager passed into NewSupervisor — NMCLI and Fake are both already
// safe for concurrent use — not the Supervisor itself, and nothing holds a
// second reference to one. A caller that starts a second goroutine calling
// Tick breaks that invariant and must add its own synchronization.
type Supervisor struct {
	mgr    Manager
	cfg    func() *config.Config
	clk    clock.Clock
	logger *slog.Logger

	// join is the gate the dashboard holds while it is joining the device to
	// a network. Nil is valid and means "no join is ever in flight" — see
	// JoinGate.
	join *JoinGate

	// feed is where every successfully read status is published, so the poll
	// loop and the dashboard can act on the same reading this supervisor
	// acted on instead of asking nmcli again. Nil is valid and means nobody
	// is listening — see StatusFeed.
	feed *StatusFeed

	// act is when the dashboard last served a request, which defers the
	// search for a saved network — see searchQuietFor. Nil is valid and means
	// nobody is ever waiting on the access point — see Activity.
	act *Activity

	// apSince is when the access point was first seen up with nothing above
	// it, or the zero time otherwise; it is what searchInterval counts from.
	// Seen, not raised: a daemon restarted under an access point the previous
	// run raised must still go looking.
	apSince time.Time

	// apSeen is the last time the access point was seen up (or raised), so a
	// forward clock step can be told from an access point that has really
	// been up that long. See clockStep.
	apSeen time.Time

	// searchUntil is when the current search for a saved network ends, or the
	// zero time when none is under way. Until then the radio is left alone
	// even though downSince is long past the grace period — that is the whole
	// point of the search, and without it the next tick would raise the
	// access point straight back.
	searchUntil time.Time

	// searchedInVain is set when a search ends with nothing found, so the
	// raise that follows is logged quietly: see the log calls in Tick.
	searchedInVain bool

	// downSince is when the device was first seen without a connection, or the
	// zero time while it is connected. It starts at construction time (see
	// NewSupervisor) rather than zero, so a device presumed down since boot
	// is recognised as overdue even if Tick is first called well after the
	// grace period. One countdown covers both the cold boot and a network
	// lost hours later, which is why there is no separate start-up case to
	// get wrong.
	downSince time.Time

	// Each of these is the clock time that operation's failure was last
	// logged, or the zero time if the last attempt succeeded (or none has
	// been made yet). They throttle logging only — see failureLogInterval —
	// never the read or the retry. Three separate fields because the three
	// operations fail independently: a board with no NetworkManager fails
	// only the status read, and must not have that suppress the first report
	// of a genuine hotspot failure later.
	lastStartFailureLog  time.Time
	lastStopFailureLog   time.Time
	lastStatusFailureLog time.Time
}

func NewSupervisor(m Manager, cfg func() *config.Config, clk clock.Clock, logger *slog.Logger, join *JoinGate, feed *StatusFeed, act *Activity) *Supervisor {
	if logger == nil {
		logger = slog.Default()
	}
	return &Supervisor{mgr: m, cfg: cfg, clk: clk, logger: logger, join: join, feed: feed, act: act, downSince: clk.Now()}
}

func (s *Supervisor) Run(ctx context.Context) {
	t := time.NewTicker(tickInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.Tick(ctx)
		}
	}
}

func (s *Supervisor) Tick(ctx context.Context) {
	now := s.clk.Now()

	// The dashboard is joining the device to a network right now: stand down
	// entirely — do not read the status, do not raise anything, do not lower
	// anything, do not advance downSince. Mid-join the radio has left the
	// access point and not yet associated, so it reports neither Connected
	// nor HotspotUp, and downSince has been set since boot: past the grace
	// period that is exactly the state the code below acts on, and it would
	// raise an access point on top of the join. The gate expires on its own,
	// so a handler that dies without releasing it costs one join's worth of
	// ticks rather than the device's ability to ever become reachable again.
	if s.join.Held(now) {
		return
	}

	ctx, cancel := context.WithTimeout(ctx, tickDeadline)
	defer cancel()

	st, err := s.mgr.Status(ctx)
	if err != nil {
		// Unknown is not the same as disconnected. Raising an access point on a
		// failed command could tear a working device off a working network.
		//
		// Throttled, and at Warn rather than Error, because the commonest cause
		// is not a fault at all: a board installed on an OS without
		// NetworkManager fails this read on every tick, forever. Unthrottled
		// that is a line every five seconds onto an SD card the spec says wears
		// out, and a permanent stream of errors about a device that is working
		// exactly as designed — announcing alerts, simply unable to configure
		// its own network — teaches an operator to ignore the log.
		s.logFailure(&s.lastStatusFailureLog, now, s.logger.Warn, "could not read the network status", err)
		return
	}
	s.lastStatusFailureLog = time.Time{}

	// Published before anything is decided on it, so whatever else reads the
	// network is reading the same tick this one acted on. Only a status the
	// manager actually returned gets here: the error path above returns
	// without publishing, because an unreadable status is not a reading of a
	// device with nothing attached.
	s.feed.Publish(st, now)

	if st.Connected {
		if !s.searchUntil.IsZero() {
			// The one outcome of a search worth a line: the device went
			// looking and found its way back.
			s.logger.Info("found a saved network while the access point was down", "ssid", st.SSID)
		}
		s.downSince = time.Time{}
		s.apSince = time.Time{}
		s.searchUntil = time.Time{}
		s.searchedInVain = false
		if st.HotspotUp {
			// downSince is already cleared above, so a StopHotspot that keeps
			// failing re-enters this branch every tick — exactly the case
			// logFailure exists to keep quiet without weakening the retry.
			if err := s.mgr.StopHotspot(ctx); err != nil {
				s.logFailure(&s.lastStopFailureLog, now, s.logger.Error, "could not lower the access point", err)
				return
			}
			s.lastStopFailureLog = time.Time{}
			s.logger.Info("joined a network, lowered the access point", "ssid", st.SSID)
		}
		return
	}

	if s.downSince.IsZero() {
		s.downSince = now
	} else if s.downSince.After(now) {
		// A clock that steps backwards must not push the access point
		// further away. These boards have no RTC, so NTP corrects the clock
		// shortly after boot — precisely when downSince matters most — and
		// clock.System.Now calls Time.In, which strips the monotonic
		// reading, so the correction is a pure backwards jump in the values
		// compared just below. The device cannot have gone down at a time
		// that has not happened yet, so a downSince in the future means the
		// clock moved, not that the device is early; that needs no arbitrary
		// threshold to detect. Re-anchoring costs at most one more grace
		// period, where leaving it would cost the whole step — hours, on a
		// device whose access point is its only way back.
		// internal/engine.Tick clamps its poll gate the same way, for the
		// same hazard on the same boards.
		s.downSince = now
	}
	if st.HotspotUp {
		// Reachable, but on a single-radio board no network can "appear"
		// while the radio serves the access point; see searchInterval.
		s.searchUntil = time.Time{}
		s.searchForSavedNetwork(ctx, now)
		return
	}
	s.apSince = time.Time{}

	if !s.searchUntil.IsZero() {
		// A search can be no longer than its window from now. A clock
		// stepped backwards mid-search would otherwise stretch it by the
		// whole step — hours with no access point and no network either.
		if s.searchUntil.Sub(now) > searchWindow {
			s.searchUntil = now.Add(searchWindow)
		}
		if now.Before(s.searchUntil) {
			return
		}
		s.searchUntil = time.Time{}
		s.searchedInVain = true
	}
	if now.Sub(s.downSince) < s.cfg().Network.APFallbackAfter.Std() {
		return
	}

	// Asked again, because the gate check at the top of this tick is stale by
	// now: the status read between them is an nmcli invocation bounded only by
	// tickDeadline, and an operator pressing Подключити during it would find
	// this tick raising an access point on top of their join — the exact
	// defect the gate exists to prevent, narrowed to one status call wide
	// rather than removed. Only the raise needs the second look; lowering a
	// hotspot mid-join is harmless.
	if s.join.Held(s.clk.Now()) {
		return
	}

	cfg := s.cfg()
	if err := s.mgr.StartHotspot(ctx, cfg.Network.APSSID, cfg.Network.APPassword); err != nil {
		// Left for the next tick to retry: a device that hit a transient error
		// at the grace boundary would otherwise stay unreachable forever.
		s.logFailure(&s.lastStartFailureLog, now, s.logger.Error, "could not raise the access point", err)
		return
	}
	s.lastStartFailureLog = time.Time{}
	s.apSince, s.apSeen = now, now
	// After a search that found nothing, the raise is the design working,
	// once every few minutes for as long as the network stays away: at Warn
	// that is hundreds of lines a day onto an SD card that wears out. The
	// first raise of an outage is the event, and keeps its Warn.
	log := s.logger.Warn
	if s.searchedInVain {
		log = s.logger.Debug
	}
	s.searchedInVain = false
	log("no network, raised the access point",
		"ssid", cfg.Network.APSSID, "down_for", now.Sub(s.downSince).Round(time.Second))
}

// searchForSavedNetwork lowers an access point that has stood alone for
// searchInterval, so NetworkManager can autoconnect to a network it
// remembers on the radio it gets back. If none answers within the window,
// the ordinary fallback in Tick raises the access point again.
//
// It is called only while the access point is up with nothing above it.
func (s *Supervisor) searchForSavedNetwork(ctx context.Context, now time.Time) {
	seen := s.apSeen
	s.apSeen = now
	if s.apSince.IsZero() || s.apSince.After(now) || now.Sub(seen) > clockStep {
		// First sight of it, or a clock that stepped. Backwards must not
		// push the search away by the whole step, as with downSince.
		// Forwards must not make an access point raised a minute ago look
		// days old: on a board with no RTC and no upstream the clock is
		// wrong, the operator on the access point is who fixes it from
		// /time, and their request was stamped before the step — so neither
		// the ceiling nor the dashboard deferral would hold the access point
		// up under the form they just submitted.
		s.apSince = now
		return
	}
	up := now.Sub(s.apSince)
	if up < searchInterval {
		return
	}
	if up < searchCeiling && s.act.Within(now, searchQuietFor) {
		return
	}
	// A board with nothing saved but its own access point can find nothing,
	// and the operator setting it up would lose their connection every few
	// minutes for it. Asked only now, once per interval, not every tick. An
	// unreadable answer searches anyway: ninety seconds of access point is
	// cheaper than every alert until somebody visits the board.
	if saved, err := s.mgr.HasSavedWiFi(ctx); err == nil && !saved {
		s.apSince = now
		return
	}
	if err := s.mgr.StopHotspot(ctx); err != nil {
		s.logFailure(&s.lastStopFailureLog, now, s.logger.Error, "could not lower the access point", err)
		return
	}
	s.lastStopFailureLog = time.Time{}
	s.searchUntil = now.Add(searchWindow)
	s.logger.Debug("no network, lowered the access point to look for a saved one",
		"up_for", up.Round(time.Second))
}

// logFailure logs a failed status read, StartHotspot or StopHotspot attempt
// through last, throttled to at most once per failureLogInterval so a device
// that keeps failing does not write a line every tick.
//
// It takes the log function rather than a level so the status read can report
// at Warn — its usual cause is a board with no NetworkManager, working
// exactly as designed — while the hotspot failures report at Error, without a
// second copy of the throttle.
//
// The first failure of a run is always logged immediately (last starts, and
// returns to, the zero time). Callers reset *last to the zero time on the
// next success, so a fault that clears and later recurs is reported again
// rather than staying suppressed by a stale timestamp from an earlier,
// unrelated run.
func (s *Supervisor) logFailure(last *time.Time, now time.Time, log func(string, ...any), msg string, err error) {
	if !last.IsZero() && now.Sub(*last) < failureLogInterval {
		return
	}
	*last = now
	log(msg, "err", err)
}
