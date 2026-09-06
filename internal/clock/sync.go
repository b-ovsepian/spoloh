package clock

import (
	"context"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// SyncChecker reports whether the host clock has been synchronised by NTP.
// ARM boards have no RTC, so the scheduler must not fire until this is true.
type SyncChecker interface {
	Synced() bool
}

// FakeSync is a fixed answer for tests.
type FakeSync struct {
	Value bool
}

func (f *FakeSync) Synced() bool { return f.Value }

// RunFunc executes a command and returns its stdout. Injected so Timedatectl
// can be tested without a systemd host.
type RunFunc func(ctx context.Context, name string, args ...string) ([]byte, error)

// syncRecheckInterval bounds how often Timedatectl shells out. The engine
// ticks once a second, so an uncached check would fork timedatectl 86,400
// times a day on a Pi 2, each fork able to block the tick for the whole of
// the 3s exec timeout below.
const syncRecheckInterval = 60 * time.Second

// syncExecTimeout caps one timedatectl invocation. D-Bus not yet up on a cold
// boot is the case this exists for: the answer then is "not synchronised",
// which is also what a missing binary or a hung bus yields.
const syncExecTimeout = 3 * time.Second

// Timedatectl asks systemd whether NTP synchronisation has completed, caching
// the answer. Use it through a pointer: the cache makes copying it wrong.
type Timedatectl struct {
	// Run replaces the exec of timedatectl. Nil means the real thing.
	Run RunFunc
	// Now replaces time.Now for the cache's own timing. Nil means the real
	// clock; tests set it to drive the re-check interval deterministically.
	Now func() time.Time

	// mu is held across the exec as well as the cache bookkeeping, so two
	// concurrent callers cannot both fork timedatectl.
	mu        sync.Mutex
	synced    bool
	nextCheck time.Time
}

// Synced reports whether the host clock is NTP-synchronised.
//
// Once it has observed a synchronised clock it short-circuits permanently. The
// scheduler's question is "does this board know what time it is", and once NTP
// has answered yes it does; timesyncd later losing sight of its server does
// not make the board's idea of the time wrong again, and treating it as though
// it did would silence the minute of silence for no gain.
//
// Until then the answer is re-checked at most once every syncRecheckInterval.
// Every failure — the binary missing, D-Bus not up, the exec timing out — is
// reported as "not synchronised", which is the safe answer but also an
// indefinite hold: callers are responsible for making that hold visible.
func (t *Timedatectl) Synced() bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.synced {
		return true
	}

	now := t.now()
	// A next-check time further out than the interval itself can only mean
	// the clock stepped backwards — which is precisely the event this type
	// exists to detect the end of, so it must not wedge the cache.
	if now.Before(t.nextCheck) && t.nextCheck.Sub(now) <= syncRecheckInterval {
		return false
	}
	t.nextCheck = now.Add(syncRecheckInterval)

	run := t.Run
	if run == nil {
		run = execRun
	}
	ctx, cancel := context.WithTimeout(context.Background(), syncExecTimeout)
	defer cancel()

	out, err := run(ctx, "timedatectl", "show", "-p", "NTPSynchronized", "--value")
	if err != nil {
		return false
	}
	t.synced = strings.TrimSpace(string(out)) == "yes"
	return t.synced
}

func (t *Timedatectl) now() time.Time {
	if t.Now != nil {
		return t.Now()
	}
	return time.Now()
}

func execRun(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).Output()
}
