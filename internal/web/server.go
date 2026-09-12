// Package web serves the device's dashboard: server-rendered HTML with every
// asset embedded, so it works on a LAN with no internet.
package web

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/b-ovsepian/spoloh/internal/configstore"
	"github.com/b-ovsepian/spoloh/internal/engine"
	"github.com/b-ovsepian/spoloh/internal/eventlog"
	"github.com/b-ovsepian/spoloh/internal/library"
	"github.com/b-ovsepian/spoloh/internal/netmgr"
	"github.com/b-ovsepian/spoloh/internal/player"
	"github.com/b-ovsepian/spoloh/internal/timedate"
)

// Deps is declared in full here even though the later tasks are what use most
// of it: an implementer reading only this task would otherwise write a struct
// that Task 9's wiring cannot fill. Nil is tolerated for everything except
// Config — the pages that need each one arrive in Tasks 7 and 8.
type Deps struct {
	Config  *configstore.Store
	Engine  *engine.Engine
	Log     *eventlog.Log
	Player  *player.Player
	Library *library.Library
	// Network drives the device's Wi-Fi: the /network page's scan, connect
	// and access-point details all read this. Task 5 wires it up from
	// main.go; nil until then, and in this package's own tests that don't
	// need it — same tolerance every other optional dependency gets.
	Network netmgr.Manager
	// JoinGate is held by /network for the length of a join, so the network
	// supervisor stands down instead of raising an access point on top of
	// the join in progress. Nil is tolerated like every other optional
	// dependency here, and a nil gate is simply never held.
	JoinGate *netmgr.JoinGate
	// NetStatus is what the network supervisor last saw. It is what decides
	// whether / is the dashboard or the network setup screen — see
	// handleHome — and it is read rather than asking Network above, which
	// would put an nmcli invocation on every page load of a screen that is
	// fetched constantly, including by the status stream's own recovery
	// probe.
	//
	// The same feed is handed to internal/engine, deliberately: the poll
	// loop pauses on exactly the condition that replaces this page, and two
	// answers to one question is how the device ends up showing a setup
	// screen while still polling, or a dashboard of frozen values with
	// nothing saying so. Nil is tolerated like every other optional
	// dependency here and reads as "not on the access point", which is the
	// ordinary dashboard.
	NetStatus *netmgr.StatusFeed
	// Devices lists the ALSA outputs the /audio page offers. player.ALSA
	// (already constructed in main.go for playback) satisfies this; nil is
	// tolerated like every other optional dependency here, and in this
	// package's own tests that don't need one.
	Devices DeviceLister
	// Timedate drives the system clock: the /time page's zone picker, its
	// NTP switch, the by-hand clock and the NTP server all read and write
	// this. nil is tolerated like every other optional dependency here, and
	// the page then says so instead of erroring.
	Timedate timedate.Manager
	// Version is the build the daemon is running, stamped into
	// cmd/spoloh's main.version by the Makefile's -ldflags and passed in
	// from there. The footer prints it, because the question "which build
	// is on that wall" is asked exactly when nobody can get a shell on the
	// device to answer it — over the phone, by whoever is standing in front
	// of it.
	//
	// Empty is tolerated like every other optional dependency, and says so
	// in words rather than rendering a blank: see versionText.
	Version string
	Logger  *slog.Logger
}

type Server struct {
	deps     Deps
	pages    pageSet
	mux      *http.ServeMux
	sessions *sessions
	limiter  *loginLimiter
	events   *Broadcaster
	// patterns records every pattern registered on mux, in registration
	// order. http.ServeMux does not expose what is on it, and a route table
	// nothing can enumerate is a route table nothing can check against the
	// authentication exemption list — see routes_test.go.
	patterns []string
	// loc caches the resolved configured time zone; see location() in sse.go.
	// It is keyed by the configured name, so a zone changed in the dashboard
	// still takes effect. The mutex is its own: this is read from the publish
	// loop and from every request handler.
	locMu sync.Mutex
	locTZ string
	loc   *time.Location

	// now stands in for time.Now so session expiry is testable from outside
	// the package via the SetNow helper in export_test.go; it defaults to
	// time.Now for every real caller. It lives on Server, not Deps, which is
	// frozen for this task.
	now func() time.Time
}

func New(d Deps) (*Server, error) {
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	s := &Server{
		deps:     d,
		mux:      http.NewServeMux(),
		sessions: newSessions(),
		limiter:  newLoginLimiter(),
		events:   NewBroadcaster(),
		now:      time.Now,
	}

	// After s exists, not before: two of the template helpers answer from
	// this server's own dependencies — the alert state in the tab title and
	// the running version in the footer — and a closure cannot be taken over
	// a value that has not been built yet. The helpers are only ever called
	// during a render, long after this returns, so closing over a
	// half-populated s here would be fine either way; building s first just
	// removes the question.
	pages, err := parseTemplates(s.funcs())
	if err != nil {
		return nil, err
	}
	s.pages = pages

	static, err := fs.Sub(staticFS, "static")
	if err != nil {
		return nil, err
	}
	s.handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(static))))
	s.handle("GET /setup", http.HandlerFunc(s.handleSetup))
	s.handle("POST /setup", http.HandlerFunc(s.handleSetup))
	s.handle("GET /login", http.HandlerFunc(s.handleLogin))
	s.handle("POST /login", http.HandlerFunc(s.handleLogin))
	s.handle("POST /logout", http.HandlerFunc(s.handleLogout))
	s.handle("GET /stream", http.HandlerFunc(s.handleEvents))
	s.handle("GET /log", http.HandlerFunc(s.handleLog))
	s.handle("POST /log/clear", http.HandlerFunc(s.handleLogClear))
	s.handle("POST /play", http.HandlerFunc(s.handlePlay))
	s.handle("POST /stop", http.HandlerFunc(s.handleStop))
	s.handle("GET /network", http.HandlerFunc(s.handleNetwork))
	s.handle("POST /network", http.HandlerFunc(s.handleNetworkConnect))
	s.handle("GET /alerts", http.HandlerFunc(s.handleAlertsPage))
	s.handle("POST /alerts", http.HandlerFunc(s.handleAlertsSave))
	s.handle("GET /events", http.HandlerFunc(s.handleEventsPage))
	s.handle("POST /events/save", http.HandlerFunc(s.handleEventsSave))
	s.handle("POST /events/add", http.HandlerFunc(s.handleEventsAdd))
	s.handle("POST /events/delete", http.HandlerFunc(s.handleEventsDelete))
	s.handle("POST /events/quiet-hours", http.HandlerFunc(s.handleEventsQuietHours))
	s.handle("GET /sounds", http.HandlerFunc(s.handleSoundsPage))
	s.handle("POST /sounds/upload", http.HandlerFunc(s.handleSoundsUpload))
	s.handle("POST /sounds/delete", http.HandlerFunc(s.handleSoundsDelete))
	s.handle("GET /audio", http.HandlerFunc(s.handleAudioPage))
	s.handle("POST /audio", http.HandlerFunc(s.handleAudioSave))
	s.handle("GET /webhooks", http.HandlerFunc(s.handleWebhooksPage))
	s.handle("POST /webhooks/add", http.HandlerFunc(s.handleWebhooksAdd))
	s.handle("POST /webhooks/save", http.HandlerFunc(s.handleWebhooksSave))
	s.handle("POST /webhooks/delete", http.HandlerFunc(s.handleWebhooksDelete))
	s.handle("GET /time", http.HandlerFunc(s.handleTimePage))
	s.handle("POST /time/timezone", http.HandlerFunc(s.handleTimeZone))
	s.handle("POST /time/ntp", http.HandlerFunc(s.handleTimeNTP))
	s.handle("POST /time/manual", http.HandlerFunc(s.handleTimeManual))
	s.handle("POST /time/server", http.HandlerFunc(s.handleTimeServer))
	s.handle("GET /settings", http.HandlerFunc(s.handleSettings))
	s.handle("GET /settings/config/export", http.HandlerFunc(s.handleConfigExport))
	s.handle("GET /{$}", http.HandlerFunc(s.handleHome))

	return s, nil
}

// handle registers a route and remembers its pattern, so the route table can
// be walked and checked against publicPaths rather than only spot-checked
// route by route. Every route must go through this and not through s.mux
// directly.
func (s *Server) handle(pattern string, h http.Handler) {
	s.mux.Handle(pattern, h)
	s.patterns = append(s.patterns, pattern)
}

// Handler returns the fully wrapped handler, middleware included. requireAuth
// wraps the whole mux rather than individual routes, so a route a later task
// adds is protected by default and has to be deliberately exempted, rather
// than protected only if whoever adds it remembers to wrap it.
func (s *Server) Handler() http.Handler {
	return securityHeaders(s.requireAuth(s.mux))
}

// publicPaths never require a session: they are how an operator reaches or
// leaves one, and static assets a login or setup page itself needs to render.
var publicPaths = map[string]bool{
	"/setup":  true,
	"/login":  true,
	"/logout": true,
}

func isPublicPath(path string) bool {
	if publicPaths[path] {
		return true
	}
	// "/static" as well as "/static/...": http.ServeMux matches the bare
	// path against the "GET /static/" pattern and answers it with a 301 to
	// the trailing-slash form, so requireAuth sees a routed request. Exempting
	// only the prefix sent that redirect to /login instead — a login page
	// where the browser expected a stylesheet.
	return path == "/static" || strings.HasPrefix(path, "/static/")
}

// securityHeaders applies the few that matter for a plain-HTTP LAN appliance:
// nothing may frame the dashboard, browsers must not sniff content types, and
// the device's URLs must not travel outward in a Referer.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

// ListenAndServe runs until ctx is cancelled, then shuts down gracefully.
func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: the SSE stream added in Task 5 is a long-lived
		// response and a write deadline would sever it every minute.
		//
		// Every request's context descends from ctx — the same one this
		// call receives and that the select below watches — rather than
		// from Background(). http.Server.Shutdown only closes idle
		// connections and then waits for active ones to finish on their
		// own; it never cancels a request context itself. A wall-mounted
		// dashboard's SSE connection is never idle, so without this,
		// handleEvents would sit in its select until something else ended
		// the connection, and Shutdown would run out the 5-second backstop
		// below on every single restart while anyone had the page open —
		// the normal case for this device, not a rare one. With
		// BaseContext wired to ctx, cancelling ctx cancels every in-flight
		// request's context in the same instant, handleEvents returns
		// immediately, and Shutdown finds nothing left to wait for.
		BaseContext: func(net.Listener) context.Context { return ctx },
	}

	// Listening (rather than calling srv.ListenAndServe, which does both at
	// once) lets a bind failure be reported before publishLoop ever starts:
	// there is no point ticking once a second and publishing to a
	// broadcaster with no server behind it.
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}

	// publishLoop is tied to the same ctx that governs this call, so it never
	// outlives the server it feeds: when ctx is done, the loop's own select
	// returns in the same tick that triggers shutdown below.
	go s.publishLoop(ctx)

	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()

	select {
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		// BaseContext above means every in-flight request has already been
		// told to stop by the time we get here, so Shutdown finding only
		// idle or already-finishing connections and returning promptly is
		// the expected path. The timeout is a backstop for whatever that
		// reasoning missed (a hijacked connection, a handler ignoring its
		// context), not the mechanism this depends on — if it ever fires,
		// force-close what is left rather than leak it for the life of the
		// process, which by then is shutting down anyway.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			srv.Close()
			return err
		}
		return nil
	}
}
