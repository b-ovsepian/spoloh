package clock_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/spoloh/spoloh/internal/clock"
)

func TestTimedatectlSynced(t *testing.T) {
	tests := []struct {
		name string
		out  string
		err  error
		want bool
	}{
		{"synchronised", "yes\n", nil, true},
		{"not synchronised", "no\n", nil, false},
		{"command missing", "", errors.New("exec: not found"), false},
		{"unexpected output", "maybe\n", nil, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := &clock.Timedatectl{
				Run: func(context.Context, string, ...string) ([]byte, error) {
					return []byte(tc.out), tc.err
				},
			}
			if got := c.Synced(); got != tc.want {
				t.Fatalf("Synced() = %v, want %v", got, tc.want)
			}
		})
	}
}

// The engine ticks once a second. Without a cache that is 86,400 forks a day
// on a Pi 2, each able to block the tick for the exec timeout.
func TestTimedatectlCachesNegativeAnswerForOneMinute(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	runs := 0
	c := &clock.Timedatectl{
		Now: func() time.Time { return now },
		Run: func(context.Context, string, ...string) ([]byte, error) {
			runs++
			return []byte("no\n"), nil
		},
	}

	for i := 0; i < 60; i++ {
		if c.Synced() {
			t.Fatal("Synced() = true, want false")
		}
		now = now.Add(time.Second)
	}
	if runs != 1 {
		t.Fatalf("ran timedatectl %d times over 59 seconds, want 1", runs)
	}

	now = now.Add(2 * time.Second) // past the 60s re-check interval
	if c.Synced() {
		t.Fatal("Synced() = true, want false")
	}
	if runs != 2 {
		t.Fatalf("ran timedatectl %d times after the interval elapsed, want 2", runs)
	}
}

// A clock that has synchronised does not un-synchronise in a way the scheduler
// cares about, so a true answer ends the shelling out for the life of the
// process.
func TestTimedatectlShortCircuitsAfterFirstTrue(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	runs := 0
	answer := "no\n"
	c := &clock.Timedatectl{
		Now: func() time.Time { return now },
		Run: func(context.Context, string, ...string) ([]byte, error) {
			runs++
			return []byte(answer), nil
		},
	}

	if c.Synced() {
		t.Fatal("Synced() = true before NTP landed")
	}
	answer = "yes\n"
	now = now.Add(61 * time.Second)
	if !c.Synced() {
		t.Fatal("Synced() = false after NTP landed")
	}
	if runs != 2 {
		t.Fatalf("runs = %d, want 2", runs)
	}

	// Hours later, and with timedatectl now answering "no" again, the answer
	// must still be true and must not have cost another fork.
	answer = "no\n"
	for i := 0; i < 10; i++ {
		now = now.Add(time.Hour)
		if !c.Synced() {
			t.Fatal("Synced() went back to false after a synchronised clock was observed")
		}
	}
	if runs != 2 {
		t.Fatalf("runs = %d after the short-circuit, want 2", runs)
	}
}

// A backwards NTP step is the event this type exists to see the end of. It
// must not push the next re-check an hour into the future and hold the
// scheduler silent for that hour.
func TestTimedatectlBackwardsClockStepDoesNotWedgeTheCache(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	runs := 0
	c := &clock.Timedatectl{
		Now: func() time.Time { return now },
		Run: func(context.Context, string, ...string) ([]byte, error) {
			runs++
			return []byte("no\n"), nil
		},
	}

	c.Synced()
	now = now.Add(-time.Hour)
	c.Synced()
	if runs != 2 {
		t.Fatalf("runs = %d after a backwards clock step, want 2", runs)
	}
}

func TestTimedatectlIsSafeForConcurrentUse(t *testing.T) {
	c := &clock.Timedatectl{
		Run: func(context.Context, string, ...string) ([]byte, error) {
			return []byte("no\n"), nil
		},
	}
	done := make(chan struct{})
	for i := 0; i < 8; i++ {
		go func() {
			for j := 0; j < 100; j++ {
				c.Synced()
			}
			done <- struct{}{}
		}()
	}
	for i := 0; i < 8; i++ {
		<-done
	}
}
