package web

import (
	"crypto/rand"
	"encoding/base64"
	"net"
	"net/http"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/b-ovsepian/spoloh/internal/config"
)

const (
	sessionCookieName = "spoloh_session"
	sessionLifetime   = 30 * 24 * time.Hour
	minPasswordLength = 10

	// A handful of attempts a minute is plenty for a person and useless for a
	// brute-force. The window is per client address.
	loginBurst  = 5
	loginWindow = time.Minute
)

type sessions struct {
	mu   sync.Mutex
	live map[string]time.Time // session id -> expiry
}

func newSessions() *sessions { return &sessions{live: map[string]time.Time{}} }

func (s *sessions) issue(now time.Time) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	id := base64.RawURLEncoding.EncodeToString(raw)

	s.mu.Lock()
	defer s.mu.Unlock()
	for existing, expiry := range s.live {
		if now.After(expiry) {
			delete(s.live, existing)
		}
	}
	s.live[id] = now.Add(sessionLifetime)
	return id, nil
}

func (s *sessions) valid(id string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	expiry, ok := s.live[id]
	if !ok {
		return false
	}
	if now.After(expiry) {
		delete(s.live, id)
		return false
	}
	return true
}

func (s *sessions) revoke(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.live, id)
}

type loginLimiter struct {
	mu       sync.Mutex
	attempts map[string][]time.Time
}

func newLoginLimiter() *loginLimiter { return &loginLimiter{attempts: map[string][]time.Time{}} }

// allow reports whether another attempt from addr may proceed. It also prunes
// addr's own history to entries still inside the window, which is what keeps
// the limiter's memory bounded per address; an attacker cycling through many
// source addresses still grows the map by one small slice per address, same
// as any per-client rate limiter.
func (l *loginLimiter) allow(addr string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	kept := l.attempts[addr][:0]
	for _, at := range l.attempts[addr] {
		if now.Sub(at) < loginWindow {
			kept = append(kept, at)
		}
	}
	if len(kept) == 0 {
		delete(l.attempts, addr)
	} else {
		l.attempts[addr] = kept
	}
	return len(kept) < loginBurst
}

func (l *loginLimiter) record(addr string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.attempts[addr] = append(l.attempts[addr], now)
}

// clear wipes an address's history. Called on a successful login so a person
// who mistyped a few times is not locked out of their own device.
func (l *loginLimiter) clear(addr string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.attempts, addr)
}

func clientAddr(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// requireAuth gates everything that is not the login, setup or static routes.
// Before a password exists it sends the operator to /setup; afterwards, to
// /login.
func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.deps.Config.Get().Auth.PasswordHash == "" {
			http.Redirect(w, r, "/setup", http.StatusSeeOther)
			return
		}
		cookie, err := r.Cookie(sessionCookieName)
		if err != nil || !s.sessions.valid(cookie.Value, s.now()) {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	// Once a password exists this route is gone, not merely redirected:
	// a redirect to /login would still answer with 3xx, and anyone probing
	// for "does this respond like a fresh device" must get nothing useful.
	// Leaving it reachable at all would let anyone on the LAN set a new
	// password and take the device over.
	if s.deps.Config.Get().Auth.PasswordHash != "" {
		http.NotFound(w, r)
		return
	}
	if r.Method == http.MethodGet {
		s.render(w, "setup.html", map[string]any{})
		return
	}

	password := r.FormValue("password")
	switch {
	case len(password) < minPasswordLength:
		s.renderSetupError(w, "Пароль має містити щонайменше 10 символів.")
		return
	case password != r.FormValue("confirm"):
		s.renderSetupError(w, "Паролі не збігаються.")
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if err := s.deps.Config.Update(func(c *config.Config) error {
		c.Auth.PasswordHash = string(hash)
		return nil
	}); err != nil {
		s.deps.Logger.Error("could not save the password", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.startSession(w, r)
}

func (s *Server) renderSetupError(w http.ResponseWriter, message string) {
	w.WriteHeader(http.StatusBadRequest)
	s.render(w, "setup.html", map[string]any{"Error": message})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	hash := s.deps.Config.Get().Auth.PasswordHash
	// No password exists yet, so there is nothing to log in against: send the
	// operator to the one route that can create one.
	if hash == "" {
		http.Redirect(w, r, "/setup", http.StatusSeeOther)
		return
	}
	if r.Method == http.MethodGet {
		s.render(w, "login.html", map[string]any{})
		return
	}

	addr := clientAddr(r)
	now := s.now()
	if !s.limiter.allow(addr, now) {
		w.WriteHeader(http.StatusTooManyRequests)
		s.render(w, "login.html", map[string]any{"Error": "Забагато спроб. Зачекайте хвилину."})
		return
	}

	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(r.FormValue("password"))) != nil {
		s.limiter.record(addr, now)
		w.WriteHeader(http.StatusUnauthorized)
		s.render(w, "login.html", map[string]any{"Error": "Невірний пароль."})
		return
	}
	s.limiter.clear(addr)
	s.startSession(w, r)
}

func (s *Server) startSession(w http.ResponseWriter, r *http.Request) {
	id, err := s.sessions.issue(s.now())
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    id,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		// Secure is deliberately unset: the dashboard is reached over plain
		// HTTP on a LAN, and a Secure cookie would never be sent at all.
		MaxAge: int(sessionLifetime.Seconds()),
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(sessionCookieName); err == nil {
		s.sessions.revoke(cookie.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookieName, Value: "", Path: "/", MaxAge: -1})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}
