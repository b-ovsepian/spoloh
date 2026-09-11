package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/b-ovsepian/spoloh/internal/config"
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

// alertStartDisabled is unexported, so the second of its two independent
// lines of defence — a *Config built some other way than config.Load, which
// is where the first line (restoring a missing alert-start event) actually
// lives — needs a test that can hand it a *config.Config directly, without
// going through configstore.Store or config.Load at all. A hand-built
// config.Config (a test fixture, an import, a future migration path) is
// exactly the case that first line cannot reach.
func TestAlertStartDisabledReportsTrueWhenTheEventIsMissingEntirely(t *testing.T) {
	cfg := config.Default()
	var withoutAlertStart []config.Event
	for _, ev := range cfg.Events {
		if ev.Trigger != config.TriggerAlertStart {
			withoutAlertStart = append(withoutAlertStart, ev)
		}
	}
	cfg.Events = withoutAlertStart

	if !alertStartDisabled(cfg) {
		t.Error("alertStartDisabled = false for a config with no alert-start event at all, want true")
	}
}
