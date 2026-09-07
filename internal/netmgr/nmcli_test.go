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
	if !byName["ShkolaWiFi"].Secured {
		t.Error("ShkolaWiFi has WPA1 WPA2 and must be reported as secured")
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
// operator joins the school network depends on Connected being false here.
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

// A password is a secret and an SSID is operator input; neither may be pasted
// into a shell. Confirm the implementation passes an argv, never a string.
func TestConnectPassesArgumentsWithoutAShell(t *testing.T) {
	rec := &recorder{}
	m := netmgr.NMCLI{Run: rec.run}

	if err := m.Connect(context.Background(), "Shkola; rm -rf /", "p4ss w0rd"); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	argv := rec.argv[0]
	var sawSSID, sawPSK bool
	for _, a := range argv {
		if a == "Shkola; rm -rf /" {
			sawSSID = true
		}
		if a == "p4ss w0rd" {
			sawPSK = true
		}
		if strings.Contains(a, "sh -c") || strings.Contains(a, "&&") {
			t.Fatalf("argument %q looks like a shell invocation", a)
		}
	}
	if !sawSSID || !sawPSK {
		t.Fatalf("argv %v does not carry the SSID and password as separate arguments", argv)
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
		{"blank line between good lines", []byte("FreeWiFi:41:\n\nShkolaWiFi:57:WPA2\n")},
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

	err := m.Connect(context.Background(), "SchoolWiFi", psk)
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
