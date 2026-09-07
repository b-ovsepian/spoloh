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

// failureLogInterval throttles repeated failure logging for StartHotspot and
// StopHotspot. The retry itself stays unconditional — a device stuck unable
// to raise or lower its access point must keep trying every tick, or it
// either stays unreachable or keeps its own AP up over a working network
// forever — but at tickInterval an unthrottled Error line would write one
// every 5 seconds indefinitely. internal/engine's heartbeatInterval throttles
// its own 5s-cadence tick the same way for the same reason: these devices run
// off SD cards that wear out (spec §5.7).
const failureLogInterval = 5 * time.Minute

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

	// downSince is when the device was first seen without a connection, or the
	// zero time while it is connected. It starts at construction time (see
	// NewSupervisor) rather than zero, so a device presumed down since boot
	// is recognised as overdue even if Tick is first called well after the
	// grace period. One countdown covers both the cold boot and a network
	// lost hours later, which is why there is no separate start-up case to
	// get wrong.
	downSince time.Time

	// lastStartFailureLog and lastStopFailureLog are the clock time each
	// operation's failure was last logged, or the zero time if the last
	// attempt succeeded (or none has been made yet). They throttle logging
	// only — see failureLogInterval — never the retry.
	lastStartFailureLog time.Time
	lastStopFailureLog  time.Time
}

func NewSupervisor(m Manager, cfg func() *config.Config, clk clock.Clock, logger *slog.Logger) *Supervisor {
	if logger == nil {
		logger = slog.Default()
	}
	return &Supervisor{mgr: m, cfg: cfg, clk: clk, logger: logger, downSince: clk.Now()}
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
	st, err := s.mgr.Status(ctx)
	if err != nil {
		// Unknown is not the same as disconnected. Raising an access point on a
		// failed command could tear a working device off a working network.
		s.logger.Warn("could not read the network status", "err", err)
		return
	}

	now := s.clk.Now()

	if st.Connected {
		s.downSince = time.Time{}
		if st.HotspotUp {
			// downSince is already cleared above, so a StopHotspot that keeps
			// failing re-enters this branch every tick — exactly the case
			// logFailure exists to keep quiet without weakening the retry.
			if err := s.mgr.StopHotspot(ctx); err != nil {
				s.logFailure(&s.lastStopFailureLog, now, "could not lower the access point", err)
				return
			}
			s.lastStopFailureLog = time.Time{}
			s.logger.Info("joined a network, lowered the access point", "ssid", st.SSID)
		}
		return
	}

	if s.downSince.IsZero() {
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
		s.logFailure(&s.lastStartFailureLog, now, "could not raise the access point", err)
		return
	}
	s.lastStartFailureLog = time.Time{}
	s.logger.Warn("no network, raised the access point",
		"ssid", cfg.Network.APSSID, "down_for", now.Sub(s.downSince).Round(time.Second))
}

// logFailure logs a failed StartHotspot or StopHotspot attempt through last,
// throttled to at most once per failureLogInterval so a device stuck
// retrying forever does not write an Error line every tick. The first
// failure of a run is always logged immediately (last starts, and returns to,
// the zero time). Callers reset *last to the zero time on the next success,
// so a fault that clears and later recurs is reported again rather than
// staying suppressed by a stale timestamp from an earlier, unrelated run.
func (s *Supervisor) logFailure(last *time.Time, now time.Time, msg string, err error) {
	if !last.IsZero() && now.Sub(*last) < failureLogInterval {
		return
	}
	*last = now
	s.logger.Error(msg, "err", err)
}
