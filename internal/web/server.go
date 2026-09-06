// Package web serves the device's dashboard: server-rendered HTML with every
// asset embedded, so it works on a LAN with no internet.
package web

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/b-ovsepian/spoloh/internal/configstore"
	"github.com/b-ovsepian/spoloh/internal/engine"
	"github.com/b-ovsepian/spoloh/internal/eventlog"
	"github.com/b-ovsepian/spoloh/internal/library"
	"github.com/b-ovsepian/spoloh/internal/player"
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
	Logger  *slog.Logger
}

type Server struct {
	deps     Deps
	pages    pageSet
	mux      *http.ServeMux
	sessions *sessions
	limiter  *loginLimiter
	events   *Broadcaster
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
	pages, err := parseTemplates()
	if err != nil {
		return nil, err
	}

	s := &Server{
		deps:     d,
		pages:    pages,
		mux:      http.NewServeMux(),
		sessions: newSessions(),
		limiter:  newLoginLimiter(),
		events:   NewBroadcaster(),
		now:      time.Now,
	}

	static, err := fs.Sub(staticFS, "static")
	if err != nil {
		return nil, err
	}
	s.mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(static))))
	s.mux.HandleFunc("GET /setup", s.handleSetup)
	s.mux.HandleFunc("POST /setup", s.handleSetup)
	s.mux.HandleFunc("GET /login", s.handleLogin)
	s.mux.HandleFunc("POST /login", s.handleLogin)
	s.mux.HandleFunc("POST /logout", s.handleLogout)
	s.mux.HandleFunc("GET /events", s.handleEvents)
	s.mux.HandleFunc("GET /log", s.handleLog)
	s.mux.HandleFunc("POST /play", s.handlePlay)
	s.mux.HandleFunc("POST /stop", s.handleStop)
	s.mux.HandleFunc("GET /{$}", s.handleHome)

	return s, nil
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
	return publicPaths[path] || strings.HasPrefix(path, "/static/")
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
	}

	// publishLoop is tied to the same ctx that governs this call, so it never
	// outlives the server it feeds: when ctx is done, the loop's own select
	// returns in the same tick that triggers shutdown below.
	go s.publishLoop(ctx)

	done := make(chan error, 1)
	go func() { done <- srv.ListenAndServe() }()

	select {
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}
