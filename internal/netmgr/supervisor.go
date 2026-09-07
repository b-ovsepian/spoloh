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

// Supervisor keeps the device reachable. It holds no belief about whether the
// hotspot is up — it reads that from the manager each tick, because a belief
// the world can contradict is a bug waiting to happen.
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

	if st.Connected {
		s.downSince = time.Time{}
		if st.HotspotUp {
			s.logger.Info("joined a network, lowering the access point", "ssid", st.SSID)
			if err := s.mgr.StopHotspot(ctx); err != nil {
				s.logger.Error("could not lower the access point", "err", err)
			}
		}
		return
	}

	now := s.clk.Now()
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
	s.logger.Warn("no network, raising the access point",
		"ssid", cfg.Network.APSSID, "down_for", now.Sub(s.downSince).Round(time.Second))
	if err := s.mgr.StartHotspot(ctx, cfg.Network.APSSID, cfg.Network.APPassword); err != nil {
		// Left for the next tick to retry: a device that hit a transient error
		// at the grace boundary would otherwise stay unreachable forever.
		s.logger.Error("could not raise the access point", "err", err)
	}
}
