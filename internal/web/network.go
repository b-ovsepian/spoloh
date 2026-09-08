package web

import (
	"context"
	"net/http"
	"sort"
	"time"

	"github.com/b-ovsepian/spoloh/internal/alertsource"
	"github.com/b-ovsepian/spoloh/internal/config"
	"github.com/b-ovsepian/spoloh/internal/netmgr"
)

// networkScanTimeout bounds how long a page load waits on a real nmcli scan,
// which can take several seconds. The operator holding the phone is standing
// right there, so on expiry the page renders with an explanatory row rather
// than hanging the request or turning into a 500 — a page listing no
// networks is still a page they can read and retry.
const networkScanTimeout = 15 * time.Second

// networkConnectTimeout bounds a join. It is far longer than the scan's: the
// operator has already committed by pressing the button, and association plus
// a DHCP lease on a school network genuinely takes tens of seconds. It exists
// to stop a wedged NetworkManager holding the goroutine forever, not to keep
// anyone waiting — see handleNetworkConnect for why the request's own
// cancellation cannot be used for that.
const networkConnectTimeout = 60 * time.Second

// networkView is what network.html reads.
type networkView struct {
	// Networks is what a scan found, strongest signal first, with any hidden
	// network (an empty SSID) already dropped: it cannot be chosen from a
	// list, so a blank row would only confuse the operator.
	Networks []netmgr.Network
	// ScanNotice explains an empty Networks list — no manager wired up yet,
	// the scan itself failing, or the 15s bound above expiring — so the page
	// never looks like it silently found nothing.
	ScanNotice string

	// Regions is the fixed 25-key list a submitted oblast is validated
	// against, and CurrentRegion is the one already configured, marked
	// selected in the picker.
	Regions       []string
	CurrentRegion string
	// SelectedSSID re-selects the network an operator just tried, when the
	// page is re-rendered after a rejected or failed submission.
	SelectedSSID string

	// APSSID and APPassword are the device's own access point, shown
	// deliberately: an operator who lost the installer's printed output has
	// nowhere else to find them. Display only — never logged, and never
	// carried in an error.
	APSSID     string
	APPassword string

	// Addresses repeats where to find the device afterwards. It is read
	// both before a submission (as part of the warning that the access
	// point is about to close) and after one succeeds, since the page the
	// operator lands on cannot be a redirect they can no longer load.
	Addresses []string

	// Error is a rejected submission's reason: an unknown region, a manager
	// that is not wired up, or the network manager's own Connect error. It
	// never carries the Wi-Fi password the operator typed.
	Error string
	// Connected switches the page from the connect form to the "you're
	// about to lose this page" success view, once Connect has actually
	// returned nil.
	Connected bool
}

// isKnownRegion reports whether region is one of alertsource.Regions()'s 25
// keys. A crafted form must not be able to write a region key that matches
// nothing upstream ever reports, which would silence the device permanently
// while the dashboard still looked configured.
func isKnownRegion(region string) bool {
	for _, r := range alertsource.Regions() {
		if r == region {
			return true
		}
	}
	return false
}

// visibleNetworksBySignal drops any hidden network (nmcli reports one in
// range as an empty SSID) and sorts the rest strongest signal first. Fake, in
// tests, returns whatever it was scripted with in whatever order that was
// written; the real NMCLI already does this same filtering and ordering
// itself, but this is what makes the page correct regardless of which
// netmgr.Manager it is handed.
func visibleNetworksBySignal(nets []netmgr.Network) []netmgr.Network {
	out := make([]netmgr.Network, 0, len(nets))
	for _, n := range nets {
		if n.SSID == "" {
			continue
		}
		out = append(out, n)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Signal > out[j].Signal })
	return out
}

// scanNetworks runs a bounded scan and turns every way it can come up empty
// — no manager wired up, the scan itself failing, or the bound above
// expiring — into an explanatory notice rather than an error page.
func (s *Server) scanNetworks(ctx context.Context) ([]netmgr.Network, string) {
	if s.deps.Network == nil {
		return nil, "Мережевий модуль недоступний на цьому пристрої."
	}

	scanCtx, cancel := context.WithTimeout(ctx, networkScanTimeout)
	defer cancel()

	nets, err := s.deps.Network.Scan(scanCtx)
	if err != nil {
		s.deps.Logger.Warn("network: scan failed", "err", err)
		return nil, "Не вдалося виконати пошук мереж. Спробуйте ще раз."
	}
	return visibleNetworksBySignal(nets), ""
}

// handleNetwork renders the page an operator sees over the device's own
// access point: what Wi-Fi networks it can see, its own access point details
// so a lost password can still be read off the screen, and the oblast
// picker.
func (s *Server) handleNetwork(w http.ResponseWriter, r *http.Request) {
	cfg := s.deps.Config.Get()
	nets, notice := s.scanNetworks(r.Context())

	view := networkView{
		Networks:      nets,
		ScanNotice:    notice,
		Regions:       alertsource.Regions(),
		CurrentRegion: cfg.Alerts.Region,
		APSSID:        cfg.Network.APSSID,
		APPassword:    cfg.Network.APPassword,
		Addresses:     LocalAddresses(),
	}
	s.render(w, http.StatusOK, "network.html", view)
}

// renderNetworkRejected re-renders the connect form after a rejected or
// failed submission, with the network list rebuilt (the page an operator
// retries from still has to show one) and the submitted ssid/region kept
// selected for convenience. It never receives or echoes the submitted
// password.
func (s *Server) renderNetworkRejected(w http.ResponseWriter, r *http.Request, status int, cfg *config.Config, ssid, region, message string) {
	nets, notice := s.scanNetworks(r.Context())
	view := networkView{
		Networks:      nets,
		ScanNotice:    notice,
		Regions:       alertsource.Regions(),
		CurrentRegion: region,
		SelectedSSID:  ssid,
		APSSID:        cfg.Network.APSSID,
		APPassword:    cfg.Network.APPassword,
		Addresses:     LocalAddresses(),
		Error:         message,
	}
	s.render(w, status, "network.html", view)
}

// handleNetworkConnect joins the device to the submitted network and, only on
// success, stores the chosen oblast.
//
// Connect first, then store the oblast: the brief requires a failed connect
// to leave the stored oblast untouched, and writing the region only after
// Connect returns nil is what guarantees that directly, with nothing to roll
// back. If the config write itself then fails after a successful join, that
// failure is deliberately left as-is rather than retried or rolled back — the
// device is already on the right network, the operator can fix the oblast
// from the settings screen, and a rollback here would only add a second
// failure mode on top of the first.
func (s *Server) handleNetworkConnect(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "cross-origin request rejected", http.StatusForbidden)
		return
	}

	ssid := r.FormValue("ssid")
	// password is never logged, never placed in a redirect URL, and never
	// echoed back into a rendered page — see renderNetworkRejected and the
	// success branch below, neither of which reads this variable.
	password := r.FormValue("password")
	region := r.FormValue("region")

	cfg := s.deps.Config.Get()

	if !isKnownRegion(region) {
		s.renderNetworkRejected(w, r, http.StatusBadRequest, cfg, ssid, cfg.Alerts.Region, "Оберіть область зі списку.")
		return
	}
	if ssid == "" {
		s.renderNetworkRejected(w, r, http.StatusBadRequest, cfg, ssid, region, "Оберіть мережу зі списку.")
		return
	}
	if s.deps.Network == nil {
		s.renderNetworkRejected(w, r, http.StatusServiceUnavailable, cfg, ssid, region, "Мережевий модуль недоступний на цьому пристрої.")
		return
	}

	// Detached from the request, on purpose. Joining the school network is
	// what drops the access point the operator submitted this form over, so
	// their browser losing the connection is the expected outcome of a
	// successful join, not a failure. Go cancels a request context as soon as
	// the client goes away, and nmcli runs under exec.CommandContext, so
	// passing r.Context() here means the join kills itself at the exact moment
	// it starts working. The deadline replaces the cancellation it removes:
	// association plus a DHCP lease can take most of a minute on a school
	// network, and a wedged NetworkManager must not leak this goroutine.
	connectCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), networkConnectTimeout)
	defer cancel()

	if err := s.deps.Network.Connect(connectCtx, ssid, password); err != nil {
		s.renderNetworkRejected(w, r, http.StatusBadGateway, cfg, ssid, region, err.Error())
		return
	}

	if err := s.deps.Config.Update(func(c *config.Config) error {
		c.Alerts.Region = region
		return nil
	}); err != nil {
		// See the doc comment above: recoverable from the settings screen,
		// not worth adding a rollback failure mode for.
		s.deps.Logger.Error("network: connected, but could not store the region", "err", err)
	}

	// No redirect: the access point that served this request is about to
	// close, so the response has to say everything the operator needs right
	// now, including where to find the device afterwards.
	s.render(w, http.StatusOK, "network.html", networkView{
		Connected: true,
		Addresses: LocalAddresses(),
	})
}
