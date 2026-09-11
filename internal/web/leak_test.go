package web_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Every private page, fetched with a live session, must be free of the two
// values that together sign session cookies. This is a build-level guard
// rather than a convention: a screen added later is covered the moment its
// route joins the table, without anyone remembering to think about it.
func TestNoPageRendersAnAuthSecret(t *testing.T) {
	const password = "correct horse battery staple"
	srv, store := newServerWithStore(t)
	setPassword(t, srv, password)
	cookie := login(t, srv, password)

	cfg := store.Get()
	secrets := map[string]string{
		"password hash":  cfg.Auth.PasswordHash,
		"session secret": cfg.Auth.SessionSecret,
	}
	for name, v := range secrets {
		if v == "" {
			t.Fatalf("%s is empty, so this test would pass against anything", name)
		}
	}

	for _, pattern := range srv.Patterns() {
		// Patterns are "METHOD /path" (see routes_test.go); strip the method
		// to get an actual request target.
		_, path, ok := strings.Cut(pattern, " ")
		if !ok {
			t.Fatalf("pattern %q carries no method; expected %q form", pattern, "METHOD /path")
		}
		path = strings.TrimSuffix(path, "{$}")
		if path == "" {
			path = "/"
		}
		// /events streams over SSE until its request context ends; bound it
		// so this walk terminates instead of hanging on that one route.
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		req := httptest.NewRequest(http.MethodGet, path, nil).WithContext(ctx)
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		cancel()

		body := rec.Body.String()
		for name, v := range secrets {
			if strings.Contains(body, v) {
				t.Errorf("%s renders the %s", path, name)
			}
		}
	}
}
