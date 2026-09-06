package web_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakePlaybackBackend is player.Backend for these tests: it never actually
// touches ALSA, just records what it was asked to do and blocks on Play until
// the request is cancelled — either by Stop or by the test's own cleanup —
// the same shape internal/player's own tests use to observe cancellation.
type fakePlaybackBackend struct {
	mu      sync.Mutex
	calls   []string
	started chan string
}

func newFakePlaybackBackend() *fakePlaybackBackend {
	return &fakePlaybackBackend{started: make(chan string, 8)}
}

func (f *fakePlaybackBackend) SetVolume(context.Context, string, int) error { return nil }

func (f *fakePlaybackBackend) Play(ctx context.Context, _, path string) error {
	name := filepath.Base(path)
	f.record("play=" + name)
	f.started <- name
	<-ctx.Done()
	f.record("cancelled=" + name)
	return ctx.Err()
}

func (f *fakePlaybackBackend) record(s string) {
	f.mu.Lock()
	f.calls = append(f.calls, s)
	f.mu.Unlock()
}

func (f *fakePlaybackBackend) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.calls))
	copy(out, f.calls)
	return out
}

func waitPlayed(t *testing.T, f *fakePlaybackBackend) string {
	t.Helper()
	select {
	case p := <-f.started:
		return p
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for playback to start")
		return ""
	}
}

// assertCancelled polls f's call log for sound's cancellation, since Stop's
// effect on the backend goroutine is asynchronous relative to the HTTP
// response that triggered it.
func assertCancelled(t *testing.T, f *fakePlaybackBackend, sound string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		for _, c := range f.Calls() {
			if c == "cancelled="+sound {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("playback was never cancelled: %v", f.Calls())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestPlayRequiresAuthentication(t *testing.T) {
	srv, _ := newServerWithStore(t)
	setPassword(t, srv, "correct horse battery staple")

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, postForm("/play", url.Values{"sound": {"siren.wav"}}))

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want a redirect to login", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/login" {
		t.Fatalf("redirected to %q, want /login — any redirect satisfies a bare status check, including one that still played the sound", loc)
	}
}

func TestPlayIsRejectedOnGet(t *testing.T) {
	srv, _ := newServerWithStore(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	req := httptest.NewRequest(http.MethodGet, "/play?sound=siren.wav", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code == http.StatusSeeOther && rec.Header().Get("Location") == "/" {
		t.Fatal("a GET played a sound; a prefetch would sound the siren")
	}
}

func TestPlaySoundsTheRequestedFile(t *testing.T) {
	srv, _, rec8 := newServerWithPlayer(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	req := postForm("/play", url.Values{"sound": {"siren.wav"}})
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303 back to the page", rec.Code)
	}
	if got := waitPlayed(t, rec8); got != "siren.wav" {
		t.Fatalf("played %q, want siren.wav", got)
	}
}

func TestPlayRejectsASoundThatIsNotInTheLibrary(t *testing.T) {
	srv, _, _ := newServerWithPlayer(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	req := postForm("/play", url.Values{"sound": {"../../etc/passwd"}})
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code == http.StatusSeeOther {
		t.Fatal("the handler accepted a name that is not a library sound")
	}
}

func TestStopSilencesPlayback(t *testing.T) {
	srv, _, rec8 := newServerWithPlayer(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	req := postForm("/play", url.Values{"sound": {"siren.wav"}})
	req.AddCookie(cookie)
	srv.Handler().ServeHTTP(httptest.NewRecorder(), req)
	waitPlayed(t, rec8)

	req = postForm("/stop", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rec.Code)
	}
	assertCancelled(t, rec8, "siren.wav")
}

// The home page trusts its own query string to build the flash notice after
// /play redirects back to it. Anyone who can get an authenticated operator to
// open a crafted "?played=" link — a link in a chat, a bookmark someone
// edited — could otherwise plant arbitrary text on the device's own
// dashboard, formatted to look like something the device said. The fix is to
// only ever echo a name that is a real library sound.
func TestFlashMessageValidatesTheSoundName(t *testing.T) {
	srv, _, _ := newServerWithPlayer(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	req := httptest.NewRequest(http.MethodGet, "/?played="+url.QueryEscape("Тривога! Це не тест"), nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if strings.Contains(rec.Body.String(), "Тривога! Це не тест") {
		t.Fatal("an unvalidated played value was echoed onto the page")
	}

	req = httptest.NewRequest(http.MethodGet, "/?played=siren.wav", nil)
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), "siren.wav") {
		t.Fatal("a real library sound was not echoed onto the page")
	}
}
