package alertsource_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/spoloh/spoloh/internal/alertsource"
)

func TestFetchSuccess(t *testing.T) {
	body := fixture(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("source"); got != "default" {
			t.Errorf("source param = %q, want default", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(body)
	}))
	defer srv.Close()

	c := alertsource.New(srv.URL, []string{"default"}, srv.Client())
	snap, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(snap.States) != 5 {
		t.Fatalf("states = %d, want 5", len(snap.States))
	}
}

func TestFetchRateLimited(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte("<html>429</html>"))
	}))
	defer srv.Close()

	c := alertsource.New(srv.URL, []string{"default"}, srv.Client())
	_, err := c.Fetch(context.Background())

	var rl *alertsource.RateLimitError
	if !errors.As(err, &rl) {
		t.Fatalf("want RateLimitError, got %v", err)
	}
	if rl.RetryAfter != 30*time.Second {
		t.Fatalf("RetryAfter = %v, want 30s", rl.RetryAfter)
	}
}

func TestFetchRejectsHTMLWith200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte("<html>maintenance</html>"))
	}))
	defer srv.Close()

	c := alertsource.New(srv.URL, []string{"default"}, srv.Client())
	if _, err := c.Fetch(context.Background()); err == nil {
		t.Fatal("an HTML body must never be treated as state")
	}
}

func TestFetchCascadesToNextSource(t *testing.T) {
	body := fixture(t)
	var tried []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		src := r.URL.Query().Get("source")
		tried = append(tried, src)
		if src == "default" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(body)
	}))
	defer srv.Close()

	c := alertsource.New(srv.URL, []string{"default", "skog"}, srv.Client())
	if _, err := c.Fetch(context.Background()); err != nil {
		t.Fatalf("Fetch should have succeeded on the second source: %v", err)
	}
	if len(tried) != 2 || tried[0] != "default" || tried[1] != "skog" {
		t.Fatalf("tried = %v, want [default skog]", tried)
	}
}

func TestFetchAllSourcesFail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := alertsource.New(srv.URL, []string{"default", "skog"}, srv.Client())
	if _, err := c.Fetch(context.Background()); err == nil {
		t.Fatal("expected an error when every source fails")
	}
}

func TestBackoff(t *testing.T) {
	tests := []struct {
		attempt int
		want    time.Duration
	}{
		{0, 15 * time.Second},
		{1, 15 * time.Second},
		{2, 30 * time.Second},
		{3, time.Minute},
		{4, 2 * time.Minute},
		{5, 4 * time.Minute},
		{6, 5 * time.Minute},
		{99, 5 * time.Minute},
	}
	for _, tc := range tests {
		if got := alertsource.Backoff(tc.attempt); got != tc.want {
			t.Errorf("Backoff(%d) = %v, want %v", tc.attempt, got, tc.want)
		}
	}
}
