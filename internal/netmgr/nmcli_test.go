package netmgr_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/b-ovsepian/spoloh/internal/netmgr"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	return data
}

// recorder captures the argv the implementation builds and returns canned output.
type recorder struct {
	out  []byte
	err  error
	argv [][]string
}

func (r *recorder) run(_ context.Context, name string, args ...string) ([]byte, error) {
	r.argv = append(r.argv, append([]string{name}, args...))
	return r.out, r.err
}

func TestScanParsesRealOutput(t *testing.T) {
	rec := &recorder{out: fixture(t, "nmcli-wifi-list.txt")}
	m := netmgr.NMCLI{Run: rec.run}

	got, err := m.Scan(context.Background())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	// A hidden network has no name to show and no way to pick it from a list,
	// so it is dropped rather than rendered as a blank row.
	for _, n := range got {
		if n.SSID == "" {
			t.Fatal("a hidden network was returned with an empty SSID")
		}
	}

	// The same SSID seen from two access points is one network to a person.
	// The stronger reading wins, because that is the one they will get.
	var kyivstar int
	for _, n := range got {
		if n.SSID == "Kyivstar_Home" {
			kyivstar++
			if n.Signal != 82 {
				t.Errorf("Kyivstar_Home signal = %d, want the stronger reading 82", n.Signal)
			}
		}
	}
	if kyivstar != 1 {
		t.Errorf("Kyivstar_Home appears %d times, want once", kyivstar)
	}

	byName := map[string]netmgr.Network{}
	for _, n := range got {
		byName[n.SSID] = n
	}
	if !byName["SiteWiFi"].Secured {
		t.Error("SiteWiFi has WPA1 WPA2 and must be reported as secured")
	}
	if byName["FreeWiFi"].Secured {
		t.Error("FreeWiFi has an empty security field and is open")
	}
}

// Strongest first: an operator picks their own network from the top of a list,
// and the strongest is almost always theirs.
func TestScanSortsBySignal(t *testing.T) {
	rec := &recorder{out: fixture(t, "nmcli-wifi-list.txt")}
	m := netmgr.NMCLI{Run: rec.run}

	got, err := m.Scan(context.Background())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	for i := 1; i < len(got); i++ {
		if got[i-1].Signal < got[i].Signal {
			t.Fatalf("results are not sorted by descending signal: %+v", got)
		}
	}
}

func TestScanUsesMachineReadableOutput(t *testing.T) {
	rec := &recorder{out: fixture(t, "nmcli-wifi-list.txt")}
	m := netmgr.NMCLI{Run: rec.run}

	if _, err := m.Scan(context.Background()); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	joined := strings.Join(rec.argv[0], " ")
	if !strings.Contains(joined, "-t") {
		t.Errorf("argv %q does not request terse output; the human table's columns shift with locale", joined)
	}
	if !strings.Contains(joined, "SSID,SIGNAL,SECURITY") {
		t.Errorf("argv %q does not pin the fields it parses", joined)
	}
}

func TestStatusParsesRealOutput(t *testing.T) {
	rec := &recorder{out: fixture(t, "nmcli-status.txt")}
	m := netmgr.NMCLI{Run: rec.run}

	got, err := m.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !got.Connected {
		t.Error("wlan0 is joined to an upstream network, so Connected must be true")
	}
	if got.SSID != "Kyivstar_Home" {
		t.Errorf("SSID = %q, want Kyivstar_Home", got.SSID)
	}
	if got.HotspotUp {
		t.Error("HotspotUp = true while joined to an upstream network, want false")
	}
}

// While the device is only serving its own access point, that is not an
// upstream connection: the supervisor's decision to call StopHotspot once an
// operator joins the site's network depends on Connected being false here.
func TestStatusReportsHotspotUp(t *testing.T) {
	rec := &recorder{out: fixture(t, "nmcli-status-hotspot.txt")}
	m := netmgr.NMCLI{Run: rec.run}

	got, err := m.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if got.Connected {
		t.Error("wlan0 is only serving the hotspot, so Connected must be false")
	}
	if !got.HotspotUp {
		t.Error("wlan0 is connected to the hotspot connection, so HotspotUp must be true")
	}
}

// A PA closet with an Ethernet port is an ordinary install. Status.Connected
// means "joined to an upstream network", and a board on a wall port is joined
// to one — reading it as disconnected raised Spoloh-Setup after 45 seconds and
// never lowered it, broadcasting a WPA2 access point indefinitely whose
// password reaches a dashboard that drives the site's speakers.
func TestStatusReportsAnEthernetUpstreamAsConnected(t *testing.T) {
	rec := &recorder{out: fixture(t, "nmcli-status-ethernet.txt")}
	m := netmgr.NMCLI{Run: rec.run}

	got, err := m.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !got.Connected {
		t.Error("eth0 is joined to an upstream network, so Connected must be true")
	}
	if got.HotspotUp {
		t.Error("HotspotUp = true with no hotspot running, want false")
	}
	// lo is connected on any modern NetworkManager. If it counted as an
	// upstream, every board would look joined and none would ever raise an
	// access point.
	if got.SSID != "" {
		t.Errorf("SSID = %q, want empty: a wired profile name is not an SSID", got.SSID)
	}
}

// Ethernet upstream plus the device's own access point still running: both
// flags at once, which is what makes the supervisor lower the access point
// instead of leaving it up over a working network.
func TestStatusReportsAnEthernetUpstreamAndAHotspotTogether(t *testing.T) {
	rec := &recorder{out: fixture(t, "nmcli-status-ethernet-hotspot.txt")}
	m := netmgr.NMCLI{Run: rec.run}

	got, err := m.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !got.Connected {
		t.Error("eth0 is joined to an upstream network, so Connected must be true")
	}
	if !got.HotspotUp {
		t.Error("wlan0 is serving spoloh-hotspot, so HotspotUp must be true")
	}
}

// A loopback device on its own is not an upstream, whatever its state says.
func TestStatusIgnoresLoopback(t *testing.T) {
	rec := &recorder{out: []byte("lo:loopback:connected:lo\nwlan0:wifi:disconnected:\n")}
	m := netmgr.NMCLI{Run: rec.run}

	got, err := m.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if got.Connected {
		t.Error("a connected loopback device was read as an upstream network")
	}
}

// A password is a secret and an SSID is operator input; neither may be pasted
// into a shell. Confirm the implementation passes an argv, never a string.
func TestConnectPassesArgumentsWithoutAShell(t *testing.T) {
	rec := &recorder{}
	m := netmgr.NMCLI{Run: rec.run}

	if err := m.Connect(context.Background(), "Site; rm -rf /", "p4ss w0rd"); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	// Every invocation, not the first: a join is three nmcli calls now —
	// drop our old profile, build one, bring it up — and the SSID and the
	// password travel in the middle one. Checking only argv[0] would have
	// passed while reading the delete.
	var sawSSID, sawPSK bool
	for _, argv := range rec.argv {
		for _, a := range argv {
			if a == "Site; rm -rf /" {
				sawSSID = true
			}
			if a == "p4ss w0rd" {
				sawPSK = true
			}
			if strings.Contains(a, "sh -c") || strings.Contains(a, "&&") {
				t.Fatalf("argument %q looks like a shell invocation", a)
			}
		}
	}
	if !sawSSID || !sawPSK {
		t.Fatalf("argv %v does not carry the SSID and password as separate arguments", rec.argv)
	}
}

// Nothing here may panic: a truncated line, a line missing fields, or empty
// output are all real shapes nmcli's output can take (a device with no
// networks in range, a mid-write pipe, a future nmcli dropping a field).
func TestScanToleratesMalformedAndEmptyOutput(t *testing.T) {
	cases := []struct {
		name string
		out  []byte
	}{
		{"empty output", []byte("")},
		{"only a newline", []byte("\n")},
		{"line with no colons", []byte("JustAnSSID")},
		{"line missing the security field", []byte("Kyivstar_Home:82")},
		{"line with only a colon", []byte(":")},
		{"trailing newline after a good line", []byte("FreeWiFi:41:\n")},
		{"blank line between good lines", []byte("FreeWiFi:41:\n\nSiteWiFi:57:WPA2\n")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := &recorder{out: c.out}
			m := netmgr.NMCLI{Run: rec.run}
			if _, err := m.Scan(context.Background()); err != nil {
				t.Fatalf("Scan: %v", err)
			}
		})
	}
}

func TestStatusToleratesMalformedAndEmptyOutput(t *testing.T) {
	cases := []struct {
		name string
		out  []byte
	}{
		{"empty output", []byte("")},
		{"line missing the connection field", []byte("wlan0:wifi:connected")},
		{"line with only a device name", []byte("wlan0")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := &recorder{out: c.out}
			m := netmgr.NMCLI{Run: rec.run}
			if _, err := m.Status(context.Background()); err != nil {
				t.Fatalf("Status: %v", err)
			}
		})
	}
}

func TestCommandFailureIsReported(t *testing.T) {
	rec := &recorder{err: errors.New("exit status 4"), out: []byte("Error: No Wi-Fi device found.")}
	m := netmgr.NMCLI{Run: rec.run}

	_, err := m.Scan(context.Background())
	if err == nil {
		t.Fatal("a failed nmcli must be an error, not an empty list")
	}
	if !strings.Contains(err.Error(), "No Wi-Fi device") {
		t.Errorf("err = %v, want it to carry nmcli's own message", err)
	}
}

// A supervisor built on top of this package will plausibly log err.Error()
// to an event log the dashboard renders. Neither the argv echo nor nmcli's
// own output (which can itself quote the failing invocation) may carry the
// Wi-Fi password back out through that error.
func TestConnectFailureRedactsPassword(t *testing.T) {
	const psk = "s3cr3t"
	rec := &recorder{
		err: errors.New("exit status 1"),
		out: []byte("Error: Connection activation failed: Secrets were required, but not provided (password s3cr3t)."),
	}
	m := netmgr.NMCLI{Run: rec.run}

	err := m.Connect(context.Background(), "SiteWiFi", psk)
	if err == nil {
		t.Fatal("a failed Connect must return an error")
	}
	msg := err.Error()
	if strings.Contains(msg, psk) {
		t.Errorf("err = %q, must not contain the password", msg)
	}
	if strings.Contains(msg, "password "+psk) {
		t.Errorf("err = %q, must not contain the argv password pair", msg)
	}
	if !strings.Contains(msg, "Secrets were required") {
		t.Errorf("err = %q, want it to still carry nmcli's own message", msg)
	}
}

// The SSID is operator-typed and "password" is a real, plausible network
// name. redactPassword must not be fooled by an SSID that happens to equal
// the keyword into redacting the SSID slot and leaving the real, final PSK
// untouched — it must anchor on the last two argv elements, not on the
// first element that happens to read "password".
func TestConnectFailureRedactsPasswordWhenSSIDIsPassword(t *testing.T) {
	const psk = "s3cr3t"
	rec := &recorder{
		err: errors.New("exit status 1"),
		out: []byte("Error: Secrets were required, but not provided."),
	}
	m := netmgr.NMCLI{Run: rec.run}

	err := m.Connect(context.Background(), "password", psk)
	if err == nil {
		t.Fatal("a failed Connect must return an error")
	}
	msg := err.Error()
	if strings.Contains(msg, psk) {
		t.Errorf("err = %q, must not contain the password", msg)
	}
}

// SSID is the first field of SSID:SIGNAL:SECURITY, so a bounded split that
// protects the last field does not protect it. An SSID containing an
// unescaped colon must not be silently misread as two fields.
func TestScanUnescapedColonInSSID(t *testing.T) {
	rec := &recorder{out: []byte("12:34:64:WPA2\n")}
	m := netmgr.NMCLI{Run: rec.run}

	got, err := m.Scan(context.Background())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d networks, want 1: %+v", len(got), got)
	}
	if got[0].SSID != "12:34" || got[0].Signal != 64 || !got[0].Secured {
		t.Errorf("got %+v, want {SSID: \"12:34\", Signal: 64, Secured: true}", got[0])
	}
}

// On real hardware nmcli's terse output most likely arrives with the colon
// escaped as \: ; the parser must handle that form too, and must unescape it
// rather than leaving a stray backslash in the SSID.
func TestScanEscapedColonInSSID(t *testing.T) {
	rec := &recorder{out: []byte(`The\:5G Network:64:WPA2` + "\n")}
	m := netmgr.NMCLI{Run: rec.run}

	got, err := m.Scan(context.Background())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d networks, want 1: %+v", len(got), got)
	}
	if got[0].SSID != "The:5G Network" || got[0].Signal != 64 {
		t.Errorf(`got %+v, want SSID "The:5G Network", signal 64`, got[0])
	}
}

// nmcli also escapes a literal backslash as \\ ; that must be unescaped back
// to a single backslash, not left doubled or passed through raw.
func TestScanEscapedBackslashInSSID(t *testing.T) {
	rec := &recorder{out: []byte(`Wi\\Fi:50:` + "\n")}
	m := netmgr.NMCLI{Run: rec.run}

	got, err := m.Scan(context.Background())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d networks, want 1: %+v", len(got), got)
	}
	if got[0].SSID != `Wi\Fi` || got[0].Secured {
		t.Errorf(`got %+v, want SSID "Wi\Fi", Secured false`, got[0])
	}
}

// CONNECTION is the variable, trailing field of DEVICE:TYPE:STATE:CONNECTION.
// An escaped colon in the connection name must not break the field count nor
// leave the name un-unescaped, since it also feeds the hotspotConnID
// comparison and would otherwise wrongly report HotspotUp.
func TestStatusEscapedColonInConnectionName(t *testing.T) {
	rec := &recorder{out: []byte(`wlan0:wifi:connected:Guest\:Net` + "\n")}
	m := netmgr.NMCLI{Run: rec.run}

	got, err := m.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !got.Connected || got.SSID != "Guest:Net" {
		t.Errorf("got %+v, want Connected true, SSID \"Guest:Net\"", got)
	}
	if got.HotspotUp {
		t.Error("got HotspotUp = true, want false")
	}
}

func TestStatusDoesNotTreatAMalformedEscapeAsTheHotspot(t *testing.T) {
	// nmcli escapes ':' and '\' and nothing else, so a backslash before any
	// other character is not an escape it wrote. Swallowing it anyway would
	// make "spoloh\-hotspot" decode to the hotspot connection name, and this
	// device would report an access point it is not running.
	rec := &recorder{out: []byte("wlan0:wifi:connected:spoloh\\-hotspot\n")}
	m := netmgr.NMCLI{Run: rec.run}

	got, err := m.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if got.HotspotUp {
		t.Error(`"spoloh\-hotspot" is not the hotspot connection, so HotspotUp must be false`)
	}
	if !got.Connected {
		t.Error("wlan0 is joined to an upstream connection, so Connected must be true")
	}
	if want := `spoloh\-hotspot`; got.SSID != want {
		t.Errorf("SSID = %q, want %q — the backslash is not an nmcli escape", got.SSID, want)
	}
}

func TestStartHotspotRefusesAnEmptyPassword(t *testing.T) {
	// nmcli treats "password" as optional, so an empty one raises an access
	// point anyone in range can join — reaching a dashboard that drives a
	// site's public address system. Refusing is the safer failure: an
	// unreachable device costs a site visit, an open one costs control of the
	// alerts themselves.
	rec := &recorder{}
	m := netmgr.NMCLI{Run: rec.run}

	err := m.StartHotspot(context.Background(), "Spoloh-Setup", "")
	if err == nil {
		t.Fatal("StartHotspot with no password must be an error, not an open access point")
	}
	if len(rec.argv) != 0 {
		t.Errorf("nmcli ran %v; nothing may be executed when the password is missing", rec.argv)
	}
}

func TestStartHotspotPassesThePasswordWithoutAShell(t *testing.T) {
	rec := &recorder{}
	m := netmgr.NMCLI{Run: rec.run}

	if err := m.StartHotspot(context.Background(), "Spoloh Setup", "sp0lohTest42"); err != nil {
		t.Fatalf("StartHotspot: %v", err)
	}
	if len(rec.argv) != 1 {
		t.Fatalf("nmcli ran %d times, want 1", len(rec.argv))
	}
	got := rec.argv[0]
	want := []string{"nmcli", "device", "wifi", "hotspot", "con-name", "spoloh-hotspot", "ssid", "Spoloh Setup", "password", "sp0lohTest42"}
	if len(got) != len(want) {
		t.Fatalf("argv = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("argv = %q, want %q", got, want)
		}
	}
}

// The join builds its own profile rather than letting nmcli find one. On the
// operator's board, `nmcli device wifi connect` reused the netplan profile
// already carrying that SSID and failed with
// "802-11-wireless-security.key-mgmt: property is missing" — from the setup
// screen the access point exists to serve.
func TestConnectBuildsItsOwnProfileWithEveryPropertyStated(t *testing.T) {
	rec := &recorder{}
	m := netmgr.NMCLI{Run: rec.run}

	if err := m.Connect(context.Background(), "OvsepianWLAN", "hunter2hunter2"); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	var flat []string
	for _, argv := range rec.argv {
		flat = append(flat, strings.Join(argv, " "))
	}
	joined := strings.Join(flat, " | ")

	if strings.Contains(joined, "device wifi connect") {
		t.Errorf("the join still asks nmcli to find a profile: %s", joined)
	}
	if !strings.Contains(joined, "802-11-wireless-security.key-mgmt wpa-psk") {
		t.Errorf("key-mgmt is not stated, which is the property nmcli would not fill in: %s", joined)
	}
	if !strings.Contains(joined, "con-name spoloh-wifi") {
		t.Errorf("the join does not use this daemon's own profile name: %s", joined)
	}
}

// Only our own profile is ever deleted. Removing whatever else claims the
// SSID would be tidier and is exactly wrong: it is someone else's
// configuration and may be how the board comes up after a power cut.
func TestConnectDeletesOnlyItsOwnProfile(t *testing.T) {
	rec := &recorder{}
	m := netmgr.NMCLI{Run: rec.run}

	if err := m.Connect(context.Background(), "OvsepianWLAN", "hunter2hunter2"); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	for _, argv := range rec.argv {
		line := strings.Join(argv, " ")
		if !strings.Contains(line, "connection delete") {
			continue
		}
		if !strings.Contains(line, "spoloh-wifi") {
			t.Errorf("the join deletes a profile that is not ours: %s", line)
		}
	}
}

// An open network has no key management to state, and stating wpa-psk for
// one would refuse a network that is genuinely open.
func TestConnectToAnOpenNetworkStatesNoSecurity(t *testing.T) {
	rec := &recorder{}
	m := netmgr.NMCLI{Run: rec.run}

	if err := m.Connect(context.Background(), "OpenGuest", ""); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	for _, argv := range rec.argv {
		if strings.Contains(strings.Join(argv, " "), "802-11-wireless-security") {
			t.Errorf("an open network was given security properties: %v", argv)
		}
	}
}

// Captured from a Raspberry Pi 4 with two saved networks: the profile the
// dashboard's join writes, and one the OS image's netplan wrote. The second's
// name was the site's real SSID and is replaced here; nothing else is.
func TestHasSavedWiFiReadsRealOutput(t *testing.T) {
	rec := &recorder{out: fixture(t, "nmcli-connections.txt")}
	m := netmgr.NMCLI{Run: rec.run}

	got, err := m.HasSavedWiFi(context.Background())
	if err != nil {
		t.Fatalf("HasSavedWiFi: %v", err)
	}
	if !got {
		t.Error("HasSavedWiFi = false with two saved Wi-Fi profiles")
	}
	want := []string{"nmcli", "-t", "-f", "TYPE,NAME", "connection", "show"}
	if len(rec.argv) != 1 || strings.Join(rec.argv[0], " ") != strings.Join(want, " ") {
		t.Errorf("argv = %v, want %v", rec.argv, want)
	}
}

// The same capture with the two saved networks removed: what a freshly
// installed board has. Its own access point is a Wi-Fi profile too, and
// counting it would send the supervisor looking for a network the device has
// never been told about, every five minutes, under the operator setting it up.
func TestHasSavedWiFiDoesNotCountTheAccessPoint(t *testing.T) {
	rec := &recorder{out: fixture(t, "nmcli-connections-none-saved.txt")}
	m := netmgr.NMCLI{Run: rec.run}

	got, err := m.HasSavedWiFi(context.Background())
	if err != nil {
		t.Fatalf("HasSavedWiFi: %v", err)
	}
	if got {
		t.Error("HasSavedWiFi = true with only the access point, loopback and a wired profile saved")
	}
}

func TestHasSavedWiFiReportsACommandFailure(t *testing.T) {
	rec := &recorder{err: errors.New("exit status 8")}
	m := netmgr.NMCLI{Run: rec.run}

	if _, err := m.HasSavedWiFi(context.Background()); err == nil {
		t.Fatal("HasSavedWiFi returned no error for a failed nmcli")
	}
}
