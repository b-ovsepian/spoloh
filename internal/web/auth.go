package web

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/bcrypt"

	"github.com/b-ovsepian/spoloh/internal/config"
)

const (
	sessionCookieName = "spoloh_session"
	sessionLifetime   = 30 * 24 * time.Hour
	minPasswordLength = 10 // in runes: the page and the input's minlength both count characters, not bytes.

	// bcrypt truncates (in some implementations) or refuses (this one) input
	// over 72 bytes. Cyrillic is two bytes per character, so a plausible
	// 37-character Ukrainian passphrase is already over the limit — this has
	// to be checked before GenerateFromPassword, not discovered from its error.
	maxPasswordBytes = 72

	// A handful of attempts a minute is plenty for a person and useless for a
	// brute-force. The window is per client address.
	loginBurst  = 5
	loginWindow = time.Minute
)

// errPasswordAlreadySet is handleSetup's signal, from inside the store's
// Update callback, that another request won the race to claim a fresh
// device. It never reaches a client directly.
var errPasswordAlreadySet = errors.New("password already set")

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
// the limiter's memory bounded: a single address never holds more than
// loginBurst timestamps, because allow refuses (without recording) once that
// many remain within the window. The map's total size is bounded by the
// number of distinct real addresses that complete a TCP handshake with the
// daemon — clientAddr reads net/http's own RemoteAddr, not a client-supplied
// header, so an attacker cannot inflate that count by claiming addresses it
// does not hold; on a LAN appliance that is the local subnet's host count,
// not an arbitrary range.
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

// sameOrigin reports whether r looks like it originated from this device's
// own pages, for the state-changing routes that require no session and so
// get no protection from SameSite cookies at all. It rejects only when a
// browser-supplied signal positively says otherwise; a request carrying
// neither header (no browser sends neither, but this package's own tests
// don't set them either) is let through rather than blocked on the absence
// of a signal that a real browser always provides.
func sameOrigin(r *http.Request) bool {
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" {
		return site == "same-origin" || site == "none"
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		u, err := url.Parse(origin)
		return err == nil && u.Host == r.Host
	}
	return true
}

// requireAuth makes every route on mux private by default: a route a later
// task adds is protected because it is on mux, not because someone remembered
// to wrap it. publicPaths is the deliberate, short exemption list. Requests
// that mux would not route to anything are passed straight through so an
// unknown path still 404s instead of announcing setup/login state.
func (s *Server) requireAuth(mux *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, pattern := mux.Handler(r); pattern == "" {
			mux.ServeHTTP(w, r)
			return
		}
		if isPublicPath(r.URL.Path) {
			mux.ServeHTTP(w, r)
			return
		}
		if s.deps.Config.Get().Auth.PasswordHash == "" {
			http.Redirect(w, r, "/setup", http.StatusSeeOther)
			return
		}
		cookie, err := r.Cookie(sessionCookieName)
		if err != nil || !s.sessions.valid(cookie.Value, s.now()) {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	// Fast path only: this is not the authoritative check. Two requests can
	// both read an empty hash here before either has written one, which is
	// why the real check-and-set happens inside the Update callback below,
	// under the store's lock.
	if s.deps.Config.Get().Auth.PasswordHash != "" {
		// A 404, not a redirect: once a password exists this route is gone,
		// and a redirect would still answer with a 3xx, the same status a
		// successful setup produces. /'s redirect to /setup already tells
		// anyone on the LAN whether the device is commissioned — hiding that
		// isn't the point — but a closed route returning anything other than
		// "not found" reads as a misleading sign that the request did something.
		http.NotFound(w, r)
		return
	}
	if r.Method == http.MethodPost && !sameOrigin(r) {
		http.Error(w, "cross-origin request rejected", http.StatusForbidden)
		return
	}
	if r.Method == http.MethodGet {
		s.render(w, http.StatusOK, "setup.html", map[string]any{})
		return
	}

	password := r.FormValue("password")
	switch {
	case utf8.RuneCountInString(password) < minPasswordLength:
		s.renderSetupError(w, "Пароль має містити щонайменше 10 символів.")
		return
	case len(password) > maxPasswordBytes:
		s.renderSetupError(w, "Пароль занадто довгий (максимум 72 байти).")
		return
	case password != r.FormValue("confirm"):
		s.renderSetupError(w, "Паролі не збігаються.")
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		s.deps.Logger.Error("could not hash the password", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	err = s.deps.Config.Update(func(c *config.Config) error {
		// The authoritative check: it runs while Update holds the store's
		// lock, so of two requests racing to claim a fresh device, only the
		// first to reach this line can still find the hash empty.
		if c.Auth.PasswordHash != "" {
			return errPasswordAlreadySet
		}
		c.Auth.PasswordHash = string(hash)
		return nil
	})
	switch {
	case errors.Is(err, errPasswordAlreadySet):
		http.NotFound(w, r)
		return
	case err != nil:
		s.deps.Logger.Error("could not save the password", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.startSession(w, r)
}

func (s *Server) renderSetupError(w http.ResponseWriter, message string) {
	s.render(w, http.StatusBadRequest, "setup.html", map[string]any{"Error": message})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	hash := s.deps.Config.Get().Auth.PasswordHash
	// No password exists yet, so there is nothing to log in against: send the
	// operator to the one route that can create one.
	if hash == "" {
		http.Redirect(w, r, "/setup", http.StatusSeeOther)
		return
	}
	if r.Method == http.MethodPost && !sameOrigin(r) {
		http.Error(w, "cross-origin request rejected", http.StatusForbidden)
		return
	}
	if r.Method == http.MethodGet {
		s.render(w, http.StatusOK, "login.html", map[string]any{})
		return
	}

	addr := clientAddr(r)
	now := s.now()
	if !s.limiter.allow(addr, now) {
		s.render(w, http.StatusTooManyRequests, "login.html", map[string]any{"Error": "Забагато спроб. Зачекайте хвилину."})
		return
	}

	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(r.FormValue("password"))) != nil {
		s.limiter.record(addr, now)
		s.render(w, http.StatusUnauthorized, "login.html", map[string]any{"Error": "Невірний пароль."})
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
