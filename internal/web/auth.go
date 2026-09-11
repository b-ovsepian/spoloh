package web

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
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

	// sessionCookieVersion prefixes every cookie this build issues, so a
	// later change to the format can be told apart from a forgery instead
	// of being guessed at.
	sessionCookieVersion = "v1"
	sessionSecretBytes   = 32
	sessionNonceBytes    = 16

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

// errNoSessionKey is startSession's signal that the device has no signing
// key yet — no password, or a secret that could not be generated. It never
// reaches a client directly.
var errNoSessionKey = errors.New("no session signing key")

// sessions issues and verifies stateless session cookies.
//
// Nothing about a live session is held in memory, deliberately. The map that
// used to hold them died with every process, and systemd restarts this unit
// as a matter of routine — Restart=always, the watchdog, and self-update later
// — so a wall-mounted dashboard was logged out several times a week. Worse,
// its SSE stream reconnected into a 303 to /login, which an EventSource treats
// as a failed connection and does not retry, leaving the wall frozen on a
// stale frame. A signed cookie removes the cause: the daemon can restart as
// often as it likes and the browser stays logged in.
//
// A cookie is "<version>.<issued unix>.<nonce>.<signature>", signed with a
// key derived from the persisted secret and the current password hash. The
// nonce makes two logins in the same second distinguishable, which is what
// lets one of them be revoked without touching the other.
//
// revoked is the one piece of memory left, and it is a deny list rather than
// the source of truth: empty means "every cookie I signed is still good",
// which is exactly the behaviour a restart should have. A logout therefore
// invalidates that cookie for the life of this process, on top of deleting it
// from the browser. What invalidates a session everywhere, permanently and
// across restarts, is changing the administrator password: the signing key is
// derived from the hash, so every cookie signed under the old one stops
// verifying. That is the lever to reach for if a cookie is believed stolen,
// and it is the one an operator would expect to work.
type sessions struct {
	mu      sync.Mutex
	revoked map[string]time.Time // cookie value -> when the entry may be forgotten
}

func newSessions() *sessions { return &sessions{revoked: map[string]time.Time{}} }

// newSessionSecret returns a fresh signing secret to persist in the config.
func newSessionSecret() (string, error) {
	raw := make([]byte, sessionSecretBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// sessionKey derives the cookie signing key. Mixing the password hash into
// it is what makes a password change log every device out; an empty secret or
// an empty hash yields no key at all, so no cookie can verify against a
// device that is not commissioned yet.
func sessionKey(secret, passwordHash string) []byte {
	if secret == "" || passwordHash == "" {
		return nil
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(passwordHash))
	return mac.Sum(nil)
}

func signSession(key []byte, payload string) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (s *sessions) issue(key []byte, now time.Time) (string, error) {
	if len(key) == 0 {
		return "", errNoSessionKey
	}
	nonce := make([]byte, sessionNonceBytes)
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	payload := sessionCookieVersion + "." +
		strconv.FormatInt(now.Unix(), 10) + "." +
		base64.RawURLEncoding.EncodeToString(nonce)
	return payload + "." + signSession(key, payload), nil
}

// valid reports whether value is a cookie this device signed, still inside
// its lifetime, and not revoked.
//
// Only an age past the lifetime is rejected, never a cookie dated in the
// future. These boards have no RTC: after a power cut the clock reads
// whatever the filesystem last recorded until systemd-timesyncd catches up,
// so a perfectly good cookie can legitimately look newer than "now" for the
// first minute of every boot. Rejecting it would log the operator out during
// exactly the window this whole change exists to protect.
func (s *sessions) valid(key []byte, value string, now time.Time) bool {
	if len(key) == 0 {
		return false
	}
	cut := strings.LastIndexByte(value, '.')
	if cut < 0 {
		return false
	}
	payload, sig := value[:cut], value[cut+1:]
	if !hmac.Equal([]byte(sig), []byte(signSession(key, payload))) {
		return false
	}
	parts := strings.Split(payload, ".")
	if len(parts) != 3 || parts[0] != sessionCookieVersion {
		return false
	}
	issued, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return false
	}
	if now.Sub(time.Unix(issued, 0)) > sessionLifetime {
		return false
	}
	return !s.isRevoked(value, now)
}

func (s *sessions) isRevoked(value string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	until, ok := s.revoked[value]
	if !ok {
		return false
	}
	if now.After(until) {
		delete(s.revoked, value)
		return false
	}
	return true
}

// revoke denies one cookie for the rest of this process's life. Callers must
// verify the cookie first: an unauthenticated /logout is otherwise a way for
// anyone on the LAN to grow this map without bound.
func (s *sessions) revoke(value string, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for existing, until := range s.revoked {
		if now.After(until) {
			delete(s.revoked, existing)
		}
	}
	// A cookie can never outlive its own issue time plus the lifetime, and
	// it was issued no later than now, so this is a safe upper bound on how
	// long the entry has to be kept.
	s.revoked[value] = now.Add(sessionLifetime)
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
		if err != nil || u.Host == "" {
			// An opaque origin — the literal "null" — or one that does not
			// parse to a host carries no information about where the request
			// came from. That is an absence of evidence, not evidence of an
			// attack, and this function's rule is to reject only on a signal
			// that positively says otherwise.
			//
			// Treating it as hostile made the dashboard unusable: Chrome sends
			// Origin: null with no Sec-Fetch-Site and no Referer for ordinary
			// same-origin form posts to this device over plain HTTP, so every
			// submission on the setup page was refused. Observed on a real
			// board, not reasoned about.
			//
			// What still protects the session routes is the SameSite=Lax
			// cookie, which a cross-site POST never carries. This check exists
			// for the two routes that have no session to protect — /setup and
			// /login — and for /setup the window is an unconfigured device
			// only, where forging a request gains an attacker nothing they
			// could not do by opening the page themselves.
			return true
		}
		return u.Host == r.Host
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
		if err != nil || !s.sessions.valid(s.sessionKey(), cookie.Value, s.now()) {
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
	// The session signing secret is born here, with the password, and is
	// written in the same atomic update: a device that has a hash but no
	// secret could issue no cookie anyone could use.
	secret, err := newSessionSecret()
	if err != nil {
		s.deps.Logger.Error("could not generate the session secret", "err", err)
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
		c.Auth.SessionSecret = secret
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

// sessionKey returns the current cookie signing key, or nil when the device
// cannot have any valid session (no password, or no secret yet).
func (s *Server) sessionKey() []byte {
	cfg := s.deps.Config.Get()
	return sessionKey(cfg.Auth.SessionSecret, cfg.Auth.PasswordHash)
}

// ensureSessionSecret backfills the secret on a device commissioned by an
// older build, which stored a password hash and kept its sessions in memory.
// handleSetup writes both at once, so this finds nothing to do on any device
// commissioned by this build.
func (s *Server) ensureSessionSecret() error {
	if s.deps.Config.Get().Auth.SessionSecret != "" {
		return nil
	}
	secret, err := newSessionSecret()
	if err != nil {
		return err
	}
	return s.deps.Config.Update(func(c *config.Config) error {
		// Another request may have won the race while we were generating;
		// its secret is as good as ours, and overwriting it would log that
		// request's own session straight back out.
		if c.Auth.SessionSecret == "" {
			c.Auth.SessionSecret = secret
		}
		return nil
	})
}

func (s *Server) startSession(w http.ResponseWriter, r *http.Request) {
	if err := s.ensureSessionSecret(); err != nil {
		s.deps.Logger.Error("could not persist the session secret", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	value, err := s.sessions.issue(s.sessionKey(), s.now())
	if err != nil {
		s.deps.Logger.Error("could not issue a session", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		// Secure is deliberately unset: the dashboard is reached over plain
		// HTTP on a LAN, and a Secure cookie would never be sent at all.
		MaxAge: int(sessionLifetime.Seconds()),
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// handleLogout ends the session in the browser and denies that exact cookie
// for the rest of this process's life. The cookie is verified first: /logout
// needs no session of its own, so accepting any string here would let anyone
// on the LAN grow the deny list without bound.
//
// A cookie signed by this device but revoked here does become usable again
// after a restart, if someone kept a copy of it — that is the price of
// sessions that survive a restart at all. The lever that invalidates a
// session everywhere and permanently is changing the administrator password,
// which changes the signing key with it.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	now := s.now()
	if cookie, err := r.Cookie(sessionCookieName); err == nil {
		if s.sessions.valid(s.sessionKey(), cookie.Value, now) {
			s.sessions.revoke(cookie.Value, now)
		}
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookieName, Value: "", Path: "/", MaxAge: -1})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}
