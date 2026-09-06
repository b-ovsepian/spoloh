// Package web serves the device's dashboard: server-rendered HTML with every
// asset embedded, so it works on a LAN with no internet.
package web

import (
	"context"
	"errors"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
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
	tmpl     *template.Template
	mux      *http.ServeMux
	sessions *sessions
	limiter  *loginLimiter
	// now stands in for time.Now so session expiry is testable; it is
	// unexported because nothing outside this package needs to set it, and
	// Deps is frozen for this task.
	now func() time.Time
}

func New(d Deps) (*Server, error) {
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	tmpl, err := parseTemplates()
	if err != nil {
		return nil, err
	}

	s := &Server{
		deps:     d,
		tmpl:     tmpl,
		mux:      http.NewServeMux(),
		sessions: newSessions(),
		limiter:  newLoginLimiter(),
		now:      time.Now,
	}

	static, err := fs.Sub(staticFS, "static")
	if err != nil {
		return nil, err
	}
	s.mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(static))))
	s.mux.HandleFunc("/setup", s.handleSetup)
	s.mux.HandleFunc("/login", s.handleLogin)
	s.mux.HandleFunc("POST /logout", s.handleLogout)
	s.mux.Handle("GET /{$}", s.requireAuth(http.HandlerFunc(s.handleIndex)))

	return s, nil
}

// handleIndex is a placeholder for the dashboard's home page: authentication
// is the only thing this task adds behind it. Later tasks replace the body.
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
}

// Handler returns the fully wrapped handler, middleware included.
func (s *Server) Handler() http.Handler {
	return securityHeaders(s.mux)
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
