package clock

import (
	"context"
	"os/exec"
	"strings"
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

// Timedatectl asks systemd whether NTP synchronisation has completed.
type Timedatectl struct {
	Run RunFunc
}

func (t Timedatectl) Synced() bool {
	run := t.Run
	if run == nil {
		run = execRun
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	out, err := run(ctx, "timedatectl", "show", "-p", "NTPSynchronized", "--value")
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) == "yes"
}

func execRun(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).Output()
}
