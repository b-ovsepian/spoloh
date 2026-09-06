package web_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
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
