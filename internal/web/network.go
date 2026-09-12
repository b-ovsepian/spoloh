package web

import (
	"context"
	"net/http"
	"sort"
	"time"

	"github.com/b-ovsepian/spoloh/internal/config"
	"github.com/b-ovsepian/spoloh/internal/netmgr"
)

// networkScanTimeout bounds how long a page load waits on a real nmcli scan,
// which can take several seconds. The operator holding the phone is standing
// right there, so on expiry the page renders with an explanatory row rather
// than hanging the request or turning into a 500 — a page listing no
// networks is still a page they can read and retry.
const networkScanTimeout = 15 * time.Second

// networkStatusTimeout bounds how long a page load waits on a real nmcli
// status query. It matches networkScanTimeout in both value and reasoning —
// the operator holding the phone is standing right there, and a wedged
// NetworkManager must not turn a page load into a hang — see that constant's
// doc comment for the full argument.
const networkStatusTimeout = 15 * time.Second

// networkConnectTimeout bounds a join. It is far longer than the scan's: the
// operator has already committed by pressing the button, and association plus
// a DHCP lease on the site's network genuinely takes tens of seconds. It exists
// to stop a wedged NetworkManager holding the goroutine forever, not to keep
// anyone waiting — see handleNetworkConnect for why the request's own
// cancellation cannot be used for that.
const networkConnectTimeout = 60 * time.Second

// joinGateSlack is added to networkConnectTimeout when this handler holds the
// join gate, so the hold outlives the join it covers rather than expiring a
// moment before Connect returns and letting the supervisor act on the last
// instant of it. It is small on purpose: the hold is released explicitly the
// moment Connect returns, so this only bounds the paths that never reach that
// release — see netmgr.JoinGate on why the hold expires at all.
const joinGateSlack = 5 * time.Second

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

	// SelectedSSID re-selects the network an operator just tried, when the
	// page is re-rendered after a rejected or failed submission.
	SelectedSSID string

	// StatusConnected, StatusSSID and StatusHotspotUp mirror netmgr.Status
	// for the "what am I on right now" section at the top of the page.
	// They are named with a Status prefix rather than reusing Connected
	// below, which already means something else here: "the join this
	// request just submitted succeeded, show the post-submit view" — a
	// live status and a one-shot submission result are different things
	// that happen to both be true on the same request in the lucky case.
	// StatusSSID is only ever non-empty when StatusConnected is true over
	// Wi-Fi; a wired upstream reports StatusConnected true with an empty
	// StatusSSID, exactly as netmgr.Status.SSID documents. StatusHotspotUp
	// is independent of StatusConnected — see netmgr.Status.HotspotUp — so
	// the page must be able to show both at once.
	StatusConnected bool
	StatusSSID      string
	StatusHotspotUp bool
	// StatusNotice explains a failed or timed-out status lookup, or a nil
	// network manager, the same way ScanNotice explains an empty scan: in
	// Ukrainian, and without taking the rest of the page down with it. When
	// set, the page must not also claim "Не підключено" — an unknown status
	// is not the same as a confirmed disconnection.
	StatusNotice string

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

	// Error is a rejected submission's reason: no network selected, a
	// manager that is not wired up, or the network manager's own Connect
	// error. It never carries the Wi-Fi password the operator typed.
	Error string
	// Connected switches the page from the connect form to the "you're
	// about to lose this page" success view, once Connect has actually
	// returned nil.
	Connected bool
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
// force asks the radio to sweep rather than reporting NetworkManager's
// cache. Only the refresh button sets it: the sweep takes seconds, and an
// ordinary page load should not spend them.
func (s *Server) scanNetworks(ctx context.Context, force bool) ([]netmgr.Network, string) {
	if s.deps.Network == nil {
		return nil, "Мережевий модуль недоступний на цьому пристрої."
	}

	scanCtx, cancel := context.WithTimeout(ctx, networkScanTimeout)
	defer cancel()

	scan := s.deps.Network.Scan
	if force {
		scan = s.deps.Network.Rescan
	}
	nets, err := scan(scanCtx)
	if err != nil {
		// A browser that left the page cancels ctx, which nmcli under
		// exec.CommandContext answers by dying to a signal — "network: scan
		// failed ... signal: killed" at Warn, on a scan that took 0.09s on
		// the real board. That is an ordinary cancellation, not a fault, and
		// logging it as one trains whoever reads the journal to ignore the
		// line that means an actual scan broke. scanCtx.Err() distinguishes
		// the two: context.Canceled means the parent (this request's own
		// context, which server.go ties to the daemon's shutdown signal as
		// well as to the client) was cancelled out from under this call,
		// where a timeout — context.DeadlineExceeded — is a real problem
		// still worth a Warn.
		if scanCtx.Err() == context.Canceled {
			s.deps.Logger.Debug("network: scan cancelled", "err", err)
		} else {
			s.deps.Logger.Warn("network: scan failed", "err", err)
		}
		return nil, "Не вдалося виконати пошук мереж. Спробуйте ще раз."
	}
	return visibleNetworksBySignal(nets), ""
}

// currentStatus runs a bounded Status call and turns every way it can fail —
// no manager wired up, the call itself failing, or the bound above expiring
// — into an explanatory Ukrainian notice, the same shape scanNetworks uses
// for the same reasons: a wedged NetworkManager must not take the rest of
// this page down with it, and an operator whose network is broken is
// exactly the operator who needs the join form to keep working.
func (s *Server) currentStatus(ctx context.Context) (netmgr.Status, string) {
	if s.deps.Network == nil {
		return netmgr.Status{}, "Мережевий модуль недоступний на цьому пристрої."
	}

	statusCtx, cancel := context.WithTimeout(ctx, networkStatusTimeout)
	defer cancel()

	st, err := s.deps.Network.Status(statusCtx)
	if err != nil {
		// Same cancellation-vs-timeout distinction scanNetworks draws, and
		// for the same reason: a browser leaving the page cancels ctx, which
		// is not a fault worth a Warn, where a wedged NetworkManager timing
		// out is.
		if statusCtx.Err() == context.Canceled {
			s.deps.Logger.Debug("network: status cancelled", "err", err)
		} else {
			s.deps.Logger.Warn("network: status failed", "err", err)
		}
		return netmgr.Status{}, "Не вдалося визначити поточне підключення."
	}
	return st, ""
}

// handleNetwork renders the page an operator sees over the device's own
// access point: what it is connected to right now, what Wi-Fi networks it
// can see, and its own access point details so a lost password can still be
// read off the screen.
func (s *Server) handleNetwork(w http.ResponseWriter, r *http.Request) {
	cfg := s.deps.Config.Get()
	// The refresh button, and nothing else. A GET rather than a form post
	// because it changes nothing on the device — it reads the air — and
	// because a link keeps working with scripts off, which every control on
	// this screen has to.
	nets, notice := s.scanNetworks(r.Context(), r.URL.Query().Has("rescan"))
	st, statusNotice := s.currentStatus(r.Context())

	view := networkView{
		Networks:        nets,
		ScanNotice:      notice,
		StatusConnected: st.Connected,
		StatusSSID:      st.SSID,
		StatusHotspotUp: st.HotspotUp,
		StatusNotice:    statusNotice,
		APSSID:          cfg.Network.APSSID,
		APPassword:      cfg.Network.APPassword,
		Addresses:       LocalAddresses(),
	}
	s.render(w, http.StatusOK, "network.html", view)
}

// renderNetworkRejected re-renders the connect form after a rejected or
// failed submission, with the network list and current status rebuilt (the
// page an operator retries from still has to show both) and the submitted
// ssid kept selected for convenience. It never receives or echoes the
// submitted password.
func (s *Server) renderNetworkRejected(w http.ResponseWriter, r *http.Request, status int, cfg *config.Config, ssid, message string) {
	nets, notice := s.scanNetworks(r.Context(), false)
	st, statusNotice := s.currentStatus(r.Context())
	view := networkView{
		Networks:        nets,
		ScanNotice:      notice,
		SelectedSSID:    ssid,
		StatusConnected: st.Connected,
		StatusSSID:      st.SSID,
		StatusHotspotUp: st.HotspotUp,
		StatusNotice:    statusNotice,
		APSSID:          cfg.Network.APSSID,
		APPassword:      cfg.Network.APPassword,
		Addresses:       LocalAddresses(),
		Error:           message,
	}
	s.render(w, status, "network.html", view)
}

// handleNetworkConnect joins the device to the submitted network.
//
// This page no longer stores anything in config: the oblast it used to write
// on a successful join moved to /alerts (see alerts.go), which is the only
// place Alerts.Region is written now. There is nothing here to roll back on
// a failed Connect any more, either — a join failure is reported and nothing
// else happens.
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

	cfg := s.deps.Config.Get()

	if ssid == "" {
		s.renderNetworkRejected(w, r, http.StatusBadRequest, cfg, ssid, "Оберіть мережу зі списку.")
		return
	}
	if s.deps.Network == nil {
		s.renderNetworkRejected(w, r, http.StatusServiceUnavailable, cfg, ssid, "Мережевий модуль недоступний на цьому пристрої.")
		return
	}

	// Detached from the request, on purpose, and from more than just the
	// client's disconnect: context.WithoutCancel strips every cancellation
	// signal r.Context() carries, including the daemon's own shutdown signal
	// — server.go's BaseContext derives every request context from
	// ListenAndServe's ctx, specifically so a restart cancels in-flight
	// requests promptly. This handler opts out of that too, not only out of
	// the client going away.
	//
	// Both cancellations are removed for the same reason: joining the site's
	// network is what drops the access point the operator submitted this
	// form over, so their browser losing the connection is the expected
	// outcome of a successful join, not a failure, and a daemon restart
	// landing mid-join is not a reason to abandon it either — nmcli runs
	// under exec.CommandContext, so either cancellation reaching this call
	// would kill the join at the exact moment it started working and could
	// leave the device on neither network. The deadline below replaces both
	// removed cancellations: association plus a DHCP lease can take most of
	// a minute on the site's network, and a wedged NetworkManager must not
	// leak this goroutine.
	//
	// This does let a detached nmcli outlive ListenAndServe's return inside
	// this process — accepted, not overlooked, because it does not outlive
	// the process itself: deploy/spoloh.service sets no KillMode, so systemd
	// defaults to KillMode=control-group and kills every process left in the
	// unit's cgroup, this one included, the moment the main process exits.
	// A future change to that unit file that sets a different KillMode would
	// invalidate this reasoning.
	connectCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), networkConnectTimeout)
	defer cancel()

	// Held from just before the join to just after it, on both paths out.
	// The supervisor ticks every five seconds and reads the radio mid-join as
	// "no network at all" — neither joined nor serving its own access point —
	// which past its grace period is exactly the state that makes it raise a
	// hotspot, on top of the join the operator is standing there waiting for.
	// The deadline is the backstop for a path that never reaches the Release
	// below; see netmgr.JoinGate for why it is a deadline and not a flag.
	// Deferred, not released inline: a panic inside Connect would otherwise
	// skip the release and leave the supervisor standing down for the whole
	// deadline. The extra time the hold covers — the render below — is
	// microseconds.
	join := s.deps.JoinGate.HoldUntil(time.Now().Add(networkConnectTimeout + joinGateSlack))
	defer s.deps.JoinGate.Release(join)

	err := s.deps.Network.Connect(connectCtx, ssid, password)
	if err != nil {
		s.renderNetworkRejected(w, r, http.StatusBadGateway, cfg, ssid, err.Error())
		return
	}

	// No redirect: the access point that served this request is about to
	// close, so the response has to say everything the operator needs right
	// now, including where to find the device afterwards.
	s.render(w, http.StatusOK, "network.html", networkView{
		Connected: true,
		Addresses: LocalAddresses(),
	})
}
