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

// Supervisor keeps the device reachable. It holds no belief about whether the
// hotspot is up — it reads that from the manager each tick, because a belief
// the world can contradict is a bug waiting to happen.
//
// Tick and Run are for a single goroutine: downSince and the failure-log
// timestamps below are read and written without synchronization. That is
// safe because the object the rest of the daemon shares across goroutines is
// the Manager passed into NewSupervisor — NMCLI and Fake are both already
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

func NewSupervisor(m Manager, cfg func() *config.Config, clk clock.Clock, logger *slog.Logger, join *JoinGate) *Supervisor {
	if logger == nil {
		logger = slog.Default()
	}
	return &Supervisor{mgr: m, cfg: cfg, clk: clk, logger: logger, join: join, downSince: clk.Now()}
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

	if st.Connected {
		s.downSince = time.Time{}
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
		return // already reachable; nothing to do until a network appears
	}
	if now.Sub(s.downSince) < s.cfg().Network.APFallbackAfter.Std() {
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
	s.logger.Warn("no network, raised the access point",
		"ssid", cfg.Network.APSSID, "down_for", now.Sub(s.downSince).Round(time.Second))
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
