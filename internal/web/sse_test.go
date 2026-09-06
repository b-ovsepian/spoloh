package web_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/b-ovsepian/spoloh/internal/web"
)

func TestEventsStreamRequiresAuthentication(t *testing.T) {
	srv, _ := newServerWithStore(t)
	setPassword(t, srv, "correct horse battery staple")

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/events", nil))

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want a redirect for an unauthenticated stream", rec.Code)
	}
}

func TestEventsStreamSendsStatusAndTerminatesWithTheRequest(t *testing.T) {
	srv, _ := newServerWithStore(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/events", nil).WithContext(ctx)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() { defer close(done); srv.Handler().ServeHTTP(rec, req) }()

	time.Sleep(200 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the stream did not end when the request was cancelled — the handler leaks a goroutine per client")
	}

	body := rec.Body.String()
	if !strings.Contains(body, "data:") {
		t.Fatalf("no SSE frame was written; body was %q", body)
	}
	if !strings.Contains(body, "\"region\"") {
		t.Fatalf("the frame does not carry the status payload; body was %q", body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("content type = %q, want text/event-stream", ct)
	}
}

// TestEventsStreamCarriesRegionWithNoEngine guards the fallback currentFrame
// shares with handleHome: before an engine exists (Deps.Engine is nil, as it
// is here and as it is for a brief window on every real boot), the stream
// must still carry the configured region rather than emitting a blank one —
// a page that briefly shows real content and then goes blank the moment the
// first tick arrives is worse than one that was blank from the start.
func TestEventsStreamCarriesRegionWithNoEngine(t *testing.T) {
	srv, _ := newServerWithStore(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/events", nil).WithContext(ctx)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() { defer close(done); srv.Handler().ServeHTTP(rec, req) }()

	time.Sleep(200 * time.Millisecond)
	cancel()
	<-done

	body := rec.Body.String()
	if !strings.Contains(body, `"region":"м. Київ"`) {
		t.Fatalf("the frame does not carry the configured region with no engine wired up; body was %q", body)
	}
}

// The publisher must survive a subscriber that never reads, because a browser
// tab suspended by the OS is exactly that.
func TestPublishDoesNotBlockOnAStalledSubscriber(t *testing.T) {
	b := web.NewBroadcaster()
	slow := b.Subscribe()
	defer b.Unsubscribe(slow)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 100; i++ {
			b.Publish([]byte("frame"))
		}
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Publish blocked on a subscriber that is not reading")
	}
}

// publishLoop skips the whole sample when nobody is subscribed, and Len is
// what tells it so. The count has to track both ends of a subscription, since
// a stale non-zero count would keep the loop working for an audience that
// left.
func TestBroadcasterCountsItsSubscribers(t *testing.T) {
	b := web.NewBroadcaster()
	if got := b.Len(); got != 0 {
		t.Fatalf("Len = %d on a fresh broadcaster, want 0", got)
	}

	first := b.Subscribe()
	second := b.Subscribe()
	if got := b.Len(); got != 2 {
		t.Fatalf("Len = %d with two subscribers, want 2", got)
	}

	b.Unsubscribe(first)
	if got := b.Len(); got != 1 {
		t.Fatalf("Len = %d after one unsubscribed, want 1", got)
	}
	// Unsubscribe is documented as safe to repeat; repeating it must not
	// drive the count below what is actually listening.
	b.Unsubscribe(first)
	b.Unsubscribe(second)
	if got := b.Len(); got != 0 {
		t.Fatalf("Len = %d once everyone left, want 0", got)
	}
}
