package main

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// discardLogger is quiet but real: serveDashboard logs on every branch, and
// a nil logger would panic rather than exercise them.
func discardLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// The failure this guards is the one seen on the board: a stale instance
// still held the port, the daemon logged the bind error once and then ran
// on with no dashboard at all — alive, unmanageable, and explicable only
// over SSH.
func TestServeDashboardRetriesUntilThePortIsFree(t *testing.T) {
	var mu sync.Mutex
	attempts := 0
	served := make(chan struct{})

	serve := func(ctx context.Context) error {
		mu.Lock()
		attempts++
		n := attempts
		mu.Unlock()
		if n < 3 {
			return errors.New("listen tcp :8080: bind: address already in use")
		}
		close(served)
		<-ctx.Done()
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		serveDashboard(ctx, serve, discardLogger())
		close(done)
	}()

	select {
	case <-served:
	case <-time.After(10 * time.Second):
		mu.Lock()
		n := attempts
		mu.Unlock()
		t.Fatalf("the dashboard never came up; serve was called %d times", n)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("serveDashboard did not return after the context was cancelled")
	}
}

// A daemon asked to stop must stop, not sit in a back-off sleep waiting to
// retry something nobody wants retried.
func TestServeDashboardStopsWhenTheContextIsCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	failed := make(chan struct{}, 1)
	serve := func(context.Context) error {
		select {
		case failed <- struct{}{}:
		default:
		}
		return errors.New("bind failed")
	}

	done := make(chan struct{})
	go func() {
		serveDashboard(ctx, serve, discardLogger())
		close(done)
	}()

	<-failed
	cancel()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("serveDashboard kept retrying after the context was cancelled")
	}
}

// The shutdown backstop must not be reported as a dashboard that could not
// come up: an operator who learns to ignore this line ignores the one that
// matters.
func TestServeDashboardReportsAShutdownFailureAsAWarning(t *testing.T) {
	var buf strings.Builder
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	serveDashboard(ctx, func(context.Context) error {
		return errors.New("shutdown backstop fired")
	}, logger)

	out := buf.String()
	if !strings.Contains(out, "level=WARN") {
		t.Errorf("a failure during shutdown was not logged at WARN; log:\n%s", out)
	}
	if strings.Contains(out, "level=ERROR") {
		t.Errorf("a failure during shutdown was logged at ERROR; log:\n%s", out)
	}
}
