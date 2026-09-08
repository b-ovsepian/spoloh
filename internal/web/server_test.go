package web_test

import (
	"bufio"
	"context"
	"io"
	"net"
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
	"github.com/b-ovsepian/spoloh/internal/netmgr"
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

// newServerWithNetwork wires a *netmgr.Fake in as Deps.Network, for
// network_test.go's exercise of the /network page against a scripted network
// manager rather than real nmcli.
func newServerWithNetwork(t *testing.T) (*web.Server, *configstore.Store, *netmgr.Fake) {
	t.Helper()
	store, err := configstore.Open(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatalf("configstore.Open: %v", err)
	}
	fake := &netmgr.Fake{}
	srv, err := web.New(web.Deps{Config: store, Network: fake})
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}
	return srv, store, fake
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
// TestListenAndServeStopsWithTheContext proves the shutdown ordering the
// daemon relies on: cancelling ctx must make ListenAndServe return promptly
// and cleanly, with no lingering listener, so main.go can safely proceed to
// stop the engine, the player, and finally close the event log.
func TestListenAndServeStopsWithTheContext(t *testing.T) {
	srv, _ := newServerWithStore(t)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- srv.ListenAndServe(ctx, "127.0.0.1:0") }()

	time.Sleep(200 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ListenAndServe returned %v, want a clean shutdown", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the server did not shut down when its context was cancelled")
	}
}

// TestListenAndServeEndsLiveConnectionsPromptlyOnShutdown guards the fix for
// the review finding that the earlier version of this test missed entirely:
// TestListenAndServeStopsWithTheContext never had a client connected, so it
// could not have caught http.Server.Shutdown's real behaviour — it closes
// idle connections and then waits indefinitely for active ones, and never
// cancels a request's own context. An SSE connection is never idle. A wall-
// mounted dashboard left open across a restart (this device's normal
// deployment) would therefore stall every shutdown for the full five-second
// backstop timeout. BaseContext fixes this by tying every request's context
// to the same ctx this call receives, so cancelling it ends the SSE handler's
// select immediately. This test proves that against a real listener and a
// real client — not an in-process ServeHTTP call — because the mechanism
// under test (a server closing a client's connection out from under it) only
// exists on the wire.
func TestListenAndServeEndsLiveConnectionsPromptlyOnShutdown(t *testing.T) {
	srv, _ := newServerWithStore(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	// Reserve a free loopback port, then release it immediately: addr is
	// needed up front to dial it, but only ListenAndServe below may bind it.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.ListenAndServe(ctx, addr) }()

	// Dial /events until the listener is actually up; retry rather than
	// sleep a fixed amount, since how long that takes is not this test's
	// concern.
	var resp *http.Response
	deadline := time.Now().Add(2 * time.Second)
	for {
		req, rerr := http.NewRequest(http.MethodGet, "http://"+addr+"/events", nil)
		if rerr != nil {
			t.Fatalf("NewRequest: %v", rerr)
		}
		req.AddCookie(cookie)
		resp, err = http.DefaultClient.Do(req)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("could not connect to the dashboard: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	defer resp.Body.Close()

	// Read the immediate frame handleEvents sends on connect, so the
	// connection is a genuinely live, held-open SSE stream — the same shape
	// a browser tab left on the home screen leaves behind — rather than one
	// that merely dialled and never received anything.
	buf := make([]byte, 64)
	if _, err := resp.Body.Read(buf); err != nil {
		t.Fatalf("reading the first SSE frame: %v", err)
	}

	start := time.Now()
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ListenAndServe returned %v, want a clean shutdown", err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("shutdown did not complete within the 5s backstop with a live SSE connection open")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("shutdown took %s with a live SSE connection open; want well under the 5s backstop, not just under it", elapsed)
	}
}

// pipeResponseWriter is a minimal http.ResponseWriter/http.Flusher backed by
// an io.Pipe. httptest.ResponseRecorder is not safe for a reader and the
// handler's writer goroutine to touch concurrently — its Buffer has no lock —
// so a long-lived stream like SSE needs something a reader can drain while
// the handler is still writing. A pipe gives that for free: Write blocks
// until Read consumes it, which is exactly the synchronization this test
// needs and nothing more.
type pipeResponseWriter struct {
	header http.Header
	pw     *io.PipeWriter
}

func newPipeResponseWriter(pw *io.PipeWriter) *pipeResponseWriter {
	return &pipeResponseWriter{header: make(http.Header), pw: pw}
}

func (p *pipeResponseWriter) Header() http.Header         { return p.header }
func (p *pipeResponseWriter) Write(b []byte) (int, error) { return p.pw.Write(b) }
func (p *pipeResponseWriter) WriteHeader(int)             {}
func (p *pipeResponseWriter) Flush()                      {}

// TestPublishLoopDeliversTicksAndStopsWithTheContext proves publishLoop's two
// unguaranteed behaviours directly, rather than by inspection: a subscriber
// actually receives a tick-driven frame while the loop runs, and the loop
// itself exits once ctx is cancelled rather than leaking past the server it
// belongs to.
func TestPublishLoopDeliversTicksAndStopsWithTheContext(t *testing.T) {
	srv, _ := newServerWithStore(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- srv.ListenAndServe(ctx, "127.0.0.1:0") }()

	req := httptest.NewRequest(http.MethodGet, "/events", nil)
	req.AddCookie(cookie)
	reqCtx, reqCancel := context.WithCancel(req.Context())
	defer reqCancel()
	req = req.WithContext(reqCtx)

	pr, pw := io.Pipe()
	w := newPipeResponseWriter(pw)
	go func() {
		srv.Handler().ServeHTTP(w, req)
		pw.Close()
	}()

	lines := make(chan string)
	readErrs := make(chan error, 1)
	go func() {
		r := bufio.NewReader(pr)
		for {
			line, err := r.ReadString('\n')
			if line != "" {
				lines <- line
			}
			if err != nil {
				readErrs <- err
				return
			}
		}
	}()

	// publishInterval is one second; wait comfortably longer than that for a
	// second, tick-driven frame — the first "data: " line is the immediate
	// snapshot handleEvents sends before the loop ever ticks.
	frames := 0
	deadline := time.After(5 * time.Second)
waitForFrames:
	for {
		select {
		case line := <-lines:
			if strings.HasPrefix(line, "data: ") {
				frames++
				if frames >= 2 {
					break waitForFrames
				}
			}
		case err := <-readErrs:
			t.Fatalf("reading the SSE stream: %v (frames seen: %d)", err, frames)
		case <-deadline:
			t.Fatalf("subscriber did not receive a tick-driven frame; frames seen: %d", frames)
		}
	}
	reqCancel()

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ListenAndServe returned %v, want a clean shutdown", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("publishLoop did not stop when its context was cancelled")
	}
}

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
