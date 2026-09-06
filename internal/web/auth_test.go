package web_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

func TestUnauthenticatedRequestRedirectsToSetupWhenNoPasswordIsSet(t *testing.T) {
	srv, _ := newServerWithStore(t)
	rec := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rec.Code)
	}
	if got := rec.Header().Get("Location"); got != "/setup" {
		t.Fatalf("Location = %q, want /setup", got)
	}
}

func TestSetupSetsThePasswordAndIsThenGone(t *testing.T) {
	srv, store := newServerWithStore(t)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, postForm("/setup", url.Values{
		"password": {"correct horse battery staple"},
		"confirm":  {"correct horse battery staple"},
	}))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303 after a successful setup", rec.Code)
	}
	if store.Get().Auth.PasswordHash == "" {
		t.Fatal("setup did not persist a password hash")
	}
	if strings.Contains(store.Get().Auth.PasswordHash, "correct horse") {
		t.Fatal("the password was stored in the clear")
	}

	// A second attempt must not be able to seize the device.
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, postForm("/setup", url.Values{
		"password": {"attacker"},
		"confirm":  {"attacker"},
	}))
	if rec.Code == http.StatusSeeOther {
		t.Fatal("/setup is still usable after a password exists")
	}
}

func TestSetupRejectsAMismatchAndAShortPassword(t *testing.T) {
	srv, store := newServerWithStore(t)

	for _, form := range []url.Values{
		{"password": {"aaaaaaaaaa"}, "confirm": {"bbbbbbbbbb"}},
		{"password": {"short"}, "confirm": {"short"}},
		{"password": {""}, "confirm": {""}},
	} {
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, postForm("/setup", form))
		if rec.Code == http.StatusSeeOther {
			t.Fatalf("setup accepted %v", form)
		}
	}
	if store.Get().Auth.PasswordHash != "" {
		t.Fatal("a rejected setup still stored a hash")
	}
}

func TestLoginGrantsASessionAndLogoutRevokesIt(t *testing.T) {
	srv, _ := newServerWithStore(t)
	setPassword(t, srv, "correct horse battery staple")

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, postForm("/login", url.Values{"password": {"correct horse battery staple"}}))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("login status = %d, want 303", rec.Code)
	}
	cookie := sessionCookie(t, rec)
	if !cookie.HttpOnly {
		t.Error("the session cookie must be HttpOnly")
	}
	if cookie.SameSite != http.SameSiteLaxMode {
		t.Error("the session cookie must be SameSite=Lax")
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("authenticated request status = %d, want 200", rec.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "/logout", nil)
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatal("the session survived logout")
	}
}

func TestLoginRejectsTheWrongPassword(t *testing.T) {
	srv, _ := newServerWithStore(t)
	setPassword(t, srv, "correct horse battery staple")

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, postForm("/login", url.Values{"password": {"wrong"}}))

	if rec.Code == http.StatusSeeOther {
		t.Fatal("login accepted the wrong password")
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == "spoloh_session" && c.Value != "" {
			t.Fatal("a session was issued for a failed login")
		}
	}
}

// Spec §7 requires the login to be rate limited; without it the password is
// brute-forceable by anyone who reaches the LAN.
func TestLoginIsRateLimited(t *testing.T) {
	srv, _ := newServerWithStore(t)
	setPassword(t, srv, "correct horse battery staple")

	var limited bool
	for i := 0; i < 20; i++ {
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, postForm("/login", url.Values{"password": {"wrong"}}))
		if rec.Code == http.StatusTooManyRequests {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatal("twenty failed logins in a row were never rate limited")
	}
}

// A rate limit that also locks out the operator holding the right password
// would turn a nuisance into a lockout on a device nobody can console into.
func TestRateLimitClearsAfterASuccessfulLogin(t *testing.T) {
	srv, _ := newServerWithStore(t)
	setPassword(t, srv, "correct horse battery staple")

	for i := 0; i < 3; i++ {
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, postForm("/login", url.Values{"password": {"wrong"}}))
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, postForm("/login", url.Values{"password": {"correct horse battery staple"}}))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want the correct password to still work after a few failures", rec.Code)
	}
}

// An operator opening the appliance for the first time has to actually see a
// form: a template bug that renders an empty body would leave a blank page
// with no way to commission the device, and no test that only checks status
// codes would ever catch it.
func TestSetupPageRendersTheForm(t *testing.T) {
	srv, _ := newServerWithStore(t)
	rec := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/setup", nil))

	body := rec.Body.String()
	if !strings.Contains(body, `name="password"`) {
		t.Errorf("setup page is missing the password field; body: %q", body)
	}
	if !strings.Contains(body, `name="confirm"`) {
		t.Errorf("setup page is missing the confirmation field; body: %q", body)
	}
}

func TestLoginPageRendersTheFormAndNothingFromSetup(t *testing.T) {
	srv, _ := newServerWithStore(t)
	setPassword(t, srv, "correct horse battery staple")

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/login", nil))

	body := rec.Body.String()
	if !strings.Contains(body, `name="password"`) {
		t.Errorf("login page is missing the password field; body: %q", body)
	}
	if strings.Contains(body, `name="confirm"`) {
		t.Errorf("login page carries setup's confirmation field; body: %q", body)
	}
}

func TestWrongPasswordBodyShowsTheError(t *testing.T) {
	srv, _ := newServerWithStore(t)
	setPassword(t, srv, "correct horse battery staple")

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, postForm("/login", url.Values{"password": {"wrong"}}))

	if !strings.Contains(rec.Body.String(), "Невірний пароль") {
		t.Errorf("body does not show the login error; body: %q", rec.Body.String())
	}
}

// The middleware's central claim is that only a session id it issued and
// still holds is accepted; nothing previously asserted that directly.
func TestForgedSessionCookieIsRejected(t *testing.T) {
	srv, _ := newServerWithStore(t)
	setPassword(t, srv, "correct horse battery staple")

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: "spoloh_session", Value: "a-forged-session-id"})
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303 for a forged session cookie", rec.Code)
	}
	if got := rec.Header().Get("Location"); got != "/login" {
		t.Fatalf("Location = %q, want /login", got)
	}
}

func TestSessionExpiresAfterItsLifetime(t *testing.T) {
	srv, _ := newServerWithStore(t)
	setPassword(t, srv, "correct horse battery staple")

	now := time.Now()
	srv.SetNow(func() time.Time { return now })

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, postForm("/login", url.Values{"password": {"correct horse battery staple"}}))
	cookie := sessionCookie(t, rec)

	now = now.Add(31 * 24 * time.Hour) // past the 30-day session lifetime

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303 once the session has expired", rec.Code)
	}
}

// bcrypt refuses input over 72 bytes; Cyrillic is two bytes per character, so
// this is a plausible passphrase length on a Ukrainian-language appliance,
// not an edge case. It must produce a real message, not a 500.
func TestSetupRejectsAPasswordOverBcryptsByteLimit(t *testing.T) {
	srv, store := newServerWithStore(t)
	long := strings.Repeat("а", 37) // 37 Cyrillic runes = 74 bytes > 72

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, postForm("/setup", url.Values{"password": {long}, "confirm": {long}}))

	if rec.Code == http.StatusInternalServerError {
		t.Fatal("an over-length password produced a bare internal error instead of a real message")
	}
	if rec.Code == http.StatusSeeOther {
		t.Fatal("setup accepted a password over bcrypt's 72-byte limit")
	}
	if store.Get().Auth.PasswordHash != "" {
		t.Fatal("a rejected setup still stored a hash")
	}
}

// The displayed rule ("щонайменше 10 символів") and the input's
// minlength="10" both count characters. The server has to count the same way
// bcrypt's byte limit is a separate, additional check, not a substitute.
func TestSetupCountsPasswordLengthInCharactersNotBytes(t *testing.T) {
	srv, store := newServerWithStore(t)
	// 7 Cyrillic characters, 14 bytes: long enough to slip past a byte-based
	// length check that means to enforce "10 characters".
	short := strings.Repeat("ї", 7)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, postForm("/setup", url.Values{"password": {short}, "confirm": {short}}))

	if rec.Code == http.StatusSeeOther {
		t.Fatal("setup accepted a 7-character password")
	}
	if store.Get().Auth.PasswordHash != "" {
		t.Fatal("a rejected setup still stored a hash")
	}
}

// configstore.Store.Update holds the store's lock for the whole callback;
// handleSetup has to do its check-and-set inside that callback; otherwise two
// requests racing a fresh device can both read an empty hash and both win.
func TestConcurrentSetupsOnlyOneWins(t *testing.T) {
	srv, store := newServerWithStore(t)

	const n = 4
	codes := make([]int, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			password := fmt.Sprintf("password number %d", i)
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, postForm("/setup", url.Values{
				"password": {password},
				"confirm":  {password},
			}))
			codes[i] = rec.Code
		}(i)
	}
	wg.Wait()

	var wins int
	for _, c := range codes {
		if c == http.StatusSeeOther {
			wins++
		}
	}
	if wins != 1 {
		t.Fatalf("got %d successful concurrent setups (codes: %v), want exactly 1", wins, codes)
	}

	hash := store.Get().Auth.PasswordHash
	var winnerFound bool
	for i := 0; i < n; i++ {
		if bcrypt.CompareHashAndPassword([]byte(hash), []byte(fmt.Sprintf("password number %d", i))) == nil {
			winnerFound = true
			break
		}
	}
	if !winnerFound {
		t.Fatal("the stored hash does not match any of the candidate passwords")
	}
}

// PUT and PATCH must not reach handlers that only expect GET or POST: Go's
// FormValue parses their bodies the same way, so a method-less route would
// let PUT /setup set the password.
func TestSetupAndLoginRejectOtherMethods(t *testing.T) {
	srv, _ := newServerWithStore(t)

	for _, path := range []string{"/setup", "/login"} {
		req := httptest.NewRequest(http.MethodPut, path, strings.NewReader("password=x&confirm=x"))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("PUT %s status = %d, want 405", path, rec.Code)
		}
	}
}

// A cross-origin page cannot use a session cookie's SameSite=Lax protection
// against it here, because these two routes need no cookie at all: a
// blind form POST from any site the operator's browser visits would
// otherwise be able to claim a fresh device or, if not fresh, spend the
// login rate limit.
func TestSetupRejectsACrossSiteOrigin(t *testing.T) {
	srv, store := newServerWithStore(t)

	req := postForm("/setup", url.Values{"password": {"correct horse battery staple"}, "confirm": {"correct horse battery staple"}})
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code == http.StatusSeeOther {
		t.Fatal("setup accepted a cross-site request")
	}
	if store.Get().Auth.PasswordHash != "" {
		t.Fatal("a cross-site setup still stored a hash")
	}
}
