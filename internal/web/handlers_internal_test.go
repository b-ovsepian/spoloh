package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// homeView is unexported, so a test that proves its MDNS field actually
// reaches the rendered page has to live in package web: web_test can only
// drive the field indirectly through handleHome, which always calls the
// real avahi socket check and would pass identically whether MDNS were
// wired to the template or deleted outright, since the test machine has no
// avahi socket either way. Rendering a homeView built by hand closes that
// hole.
func TestHomeViewMDNSFieldReachesTheTemplate(t *testing.T) {
	srv, err := New(Deps{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	for _, tc := range []struct {
		name string
		mdns string
		want bool
	}{
		{"set", "spoloh.local", true},
		{"empty", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			view := homeView{Addresses: []string{"10.0.0.5"}, MDNS: tc.mdns}
			rec := httptest.NewRecorder()
			srv.render(rec, http.StatusOK, "home.html", view)

			got := strings.Contains(rec.Body.String(), `<p class="font-mono text-2xl">spoloh.local</p>`)
			if got != tc.want {
				t.Errorf("homeView.MDNS = %q: page contains the rendered name = %v, want %v", tc.mdns, got, tc.want)
			}
		})
	}
}
