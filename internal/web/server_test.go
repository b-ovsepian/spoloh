package web_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/b-ovsepian/spoloh/internal/clock"
	"github.com/b-ovsepian/spoloh/internal/configstore"
	"github.com/b-ovsepian/spoloh/internal/engine"
	"github.com/b-ovsepian/spoloh/internal/eventlog"
	"github.com/b-ovsepian/spoloh/internal/library"
	"github.com/b-ovsepian/spoloh/internal/player"
	"github.com/b-ovsepian/spoloh/internal/web"
)

func newServerWithStore(t *testing.T) (*web.Server, *configstore.Store) {
	t.Helper()
	store, err := configstore.Open(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatalf("configstore.Open: %v", err)
	}
	srv, err := web.New(web.Deps{Config: store})
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}
	return srv, store
}

// newServerWithLog wires up a real eventlog.Log backed by a temp file, for
// tests that need the /log page to have something to render.
func newServerWithLog(t *testing.T) (*web.Server, *configstore.Store, *eventlog.Log) {
	t.Helper()
	store, err := configstore.Open(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatalf("configstore.Open: %v", err)
	}
	lg, err := eventlog.Open(filepath.Join(t.TempDir(), "spoloh.db"), 100)
	if err != nil {
		t.Fatalf("eventlog.Open: %v", err)
	}
	t.Cleanup(func() { lg.Close() })
	srv, err := web.New(web.Deps{Config: store, Log: lg})
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}
	return srv, store, lg
}

// newServerWithPlayer wires a real engine.Engine and player.Player over a
// recording backend — the same shape internal/engine's own tests use — so
// /play and /stop exercise the actual manual-playback path (PlayManual, the
// library lookup, the player's queue and cancellation) rather than a mock
// standing in for it. The returned *fakePlaybackBackend is what manual_test.go
// inspects.
func newServerWithPlayer(t *testing.T) (*web.Server, *configstore.Store, *fakePlaybackBackend) {
	t.Helper()
	store, err := configstore.Open(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatalf("configstore.Open: %v", err)
	}

	soundsDir := t.TempDir()
	riff := make([]byte, 64)
	copy(riff, "RIFF")
	copy(riff[8:], "WAVE")
	if err := os.WriteFile(filepath.Join(soundsDir, "siren.wav"), riff, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	lib := library.New(soundsDir, library.FFmpeg{})

	backend := newFakePlaybackBackend()
	pl := player.New(backend, "default")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go pl.Run(ctx)

	eng := engine.New(engine.Deps{
		Config:  store.Get,
		Player:  pl,
		Library: lib,
		Clock:   clock.System{Loc: time.UTC},
	})

	srv, err := web.New(web.Deps{Config: store, Engine: eng, Player: pl, Library: lib})
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}
	return srv, store, backend
}

func appendAlert(t *testing.T, lg *eventlog.Log, sound string) {
	t.Helper()
	if err := lg.Append(eventlog.Entry{Kind: eventlog.KindAlert, Sound: sound, OK: true}); err != nil {
		t.Fatalf("Append: %v", err)
	}
}

func newServer(t *testing.T) *web.Server {
	t.Helper()
	srv, _ := newServerWithStore(t)
	return srv
}

func postForm(path string, form url.Values) *http.Request {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return req
}

func setPassword(t *testing.T, srv *web.Server, password string) {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, postForm("/setup", url.Values{"password": {password}, "confirm": {password}}))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("setup failed: status %d", rec.Code)
	}
}

func login(t *testing.T, srv *web.Server, password string) *http.Cookie {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, postForm("/login", url.Values{"password": {password}}))
	return sessionCookie(t, rec)
}

func sessionCookie(t *testing.T, rec *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == "spoloh_session" {
			return c
		}
	}
	t.Fatal("no session cookie was issued")
	return nil
}

func TestStaticAssetsAreServedFromTheBinary(t *testing.T) {
	srv := newServer(t)
	rec := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/static/app.css", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "css") {
		t.Fatalf("content type = %q, want a CSS type", ct)
	}
	if rec.Body.Len() == 0 {
		t.Fatal("the stylesheet is empty — was it built and committed?")
	}
}

func TestUnknownPathIsNotFound(t *testing.T) {
	srv := newServer(t)
	rec := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/nope", nil))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// The dashboard is served over plain HTTP on a LAN, so it must not be
// embeddable in someone else's page and must not leak its URLs onward.
func TestSecurityHeadersArePresent(t *testing.T) {
	srv := newServer(t)
	rec := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/static/app.css", nil))

	for header, want := range map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "no-referrer",
	} {
		if got := rec.Header().Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
}
