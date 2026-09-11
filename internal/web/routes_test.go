package web_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/b-ovsepian/spoloh/internal/web"
)

// TestEveryRouteIsPublicOrPrivateAsIntended walks the whole route table and
// rules on each entry, rather than spot-checking the two routes somebody
// happened to write a test for. requireAuth's default-deny makes the current
// set structurally safe, but nothing asserted that, and nothing would notice
// a later task adding a path to publicPaths without meaning to. This test
// fails on an unruled route, so Plan 2b's screens have to state their
// intent here before they can be merged.
func TestEveryRouteIsPublicOrPrivateAsIntended(t *testing.T) {
	srv, _ := newServerWithStore(t)
	setPassword(t, srv, "correct horse battery staple")

	// The ruling. Public means "reachable with no session": the routes by
	// which an operator enters or leaves one, plus the assets a login or
	// setup page needs in order to render at all.
	public := map[string]bool{
		"/setup":   true,
		"/login":   true,
		"/logout":  true,
		"/static/": true,

		"/":                   false,
		"/stream":             false,
		"/log":                false,
		"/log/clear":          false,
		"/play":               false,
		"/stop":               false,
		"/network":            false,
		"/alerts":             false,
		"/events":             false,
		"/events/save":        false,
		"/events/add":         false,
		"/events/delete":      false,
		"/events/quiet-hours": false,
		"/sounds":             false,
		"/sounds/upload":      false,
		"/sounds/delete":      false,
		"/audio":              false,
		"/time":               false,
		"/time/timezone":      false,
		"/time/ntp":           false,
		"/time/manual":        false,
		"/time/server":        false,
	}

	seen := map[string]bool{}
	for _, pattern := range srv.Patterns() {
		_, path, ok := strings.Cut(pattern, " ")
		if !ok {
			t.Fatalf("pattern %q carries no method; the table below assumes %q form", pattern, "METHOD /path")
		}
		// "GET /{$}" is the exact-match spelling of the home page.
		path = strings.TrimSuffix(path, "{$}")
		seen[path] = true

		want, ruled := public[path]
		if !ruled {
			t.Errorf("route %q has no ruling: add it to this test as public or private, deliberately", pattern)
			continue
		}
		if got := web.IsPublicPath(path); got != want {
			t.Errorf("route %q: IsPublicPath(%q) = %v, want %v", pattern, path, got, want)
		}
	}
	for path := range public {
		if !seen[path] {
			t.Errorf("this test rules on %q, but no route registers it any more", path)
		}
	}

	// The ruling has to hold in the middleware too, not just in the predicate
	// it consults. A private route reached with no session must send the
	// operator to the login page and nowhere else.
	for path, isPublic := range public {
		if isPublic {
			continue
		}
		method := http.MethodGet
		switch path {
		case "/log/clear", "/play", "/stop", "/events/save", "/events/add", "/events/delete", "/events/quiet-hours", "/sounds/upload", "/sounds/delete",
			"/time/timezone", "/time/ntp", "/time/manual", "/time/server":
			method = http.MethodPost
		}
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, httptest.NewRequest(method, path, nil))
		if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login" {
			t.Errorf("%s %s with no session = %d %q, want 303 to /login",
				method, path, rec.Code, rec.Header().Get("Location"))
		}
	}
}

// The exemption is a prefix, "/static/", but http.ServeMux matches the bare
// "/static" against the "GET /static/" pattern and answers it with a 301 to
// the trailing-slash form. requireAuth therefore saw a routed request that
// the prefix did not cover, and sent it to /login — a login page where the
// browser expected a stylesheet.
func TestTheStaticPrefixCoversThePathWithoutItsTrailingSlash(t *testing.T) {
	srv, _ := newServerWithStore(t)
	setPassword(t, srv, "correct horse battery staple")

	if !web.IsPublicPath("/static") {
		t.Error(`IsPublicPath("/static") = false; the bare path is routed and so must be exempt too`)
	}

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/static", nil))
	if loc := rec.Header().Get("Location"); loc == "/login" {
		t.Errorf("GET /static with no session redirected to %q, want the mux's own redirect to /static/", loc)
	}
}
