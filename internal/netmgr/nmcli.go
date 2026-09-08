package netmgr

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"
)

// RunFunc executes a command and returns its combined output. Injected so
// NMCLI can be tested without a NetworkManager host, matching the pattern
// internal/clock uses for timedatectl.
type RunFunc func(ctx context.Context, name string, args ...string) ([]byte, error)

// hotspotConnID is the connection profile name NMCLI gives every hotspot it
// starts. Naming it ourselves, rather than relying on nmcli's own default, is
// what lets StopHotspot bring the right profile down deterministically, and
// what lets Status tell a hotspot apart from an ordinary upstream connection
// by name. "Hotspot" was rejected as a plausible name an operator could
// already have configured before this daemon ever ran; namespacing it like
// the rest of the project avoids that collision.
//
// All three of StartHotspot, StopHotspot and Status read this one constant.
// Naming the same connection by a literal in more than one place is how they
// drift apart.
//
// This does not fully rule out the collision it namespaces against: an
// operator could already have a connection profile literally named
// "spoloh-hotspot" before this daemon ever ran, in which case Status and
// StopHotspot would treat that pre-existing profile as ours. Namespacing
// only makes the collision unlikely, not impossible.
const hotspotConnID = "spoloh-hotspot"

// NMCLI drives NetworkManager through the nmcli binary. It holds no state of
// its own beyond its configuration, so a value is fine to pass around.
type NMCLI struct {
	// Bin is the nmcli executable to run. Empty means "nmcli" on PATH.
	Bin string
	// Run replaces the exec of nmcli. Nil means the real thing.
	Run RunFunc
}

var _ Manager = NMCLI{}

func (m NMCLI) bin() string {
	if m.Bin != "" {
		return m.Bin
	}
	return "nmcli"
}

// run executes nmcli with args and returns its output. On failure the error
// carries nmcli's own combined output: that message is the only thing that
// will tell an operator what actually went wrong (missing device, wrong
// password, rfkill blocked, ...), so it must not be discarded.
//
// This is also exactly the message a caller is likely to log: the supervisor
// built on top of this package logs errors like this one to an event log
// that a dashboard renders for anyone who can open it. Connect and
// StartHotspot pass the Wi-Fi PSK as the argv element right after
// "password", and nmcli itself sometimes echoes its own failing invocation
// back in its output, so both the argv this function echoes and nmcli's own
// message are redacted before either is wrapped into the returned error.
func (m NMCLI) run(ctx context.Context, args ...string) ([]byte, error) {
	run := m.Run
	if run == nil {
		run = execRun
	}
	out, err := run(ctx, m.bin(), args...)
	if err != nil {
		secret, redactedArgs := redactPassword(args)
		msg := strings.TrimSpace(string(out))
		if secret != "" {
			msg = strings.ReplaceAll(msg, secret, "***")
		}
		if msg == "" {
			return out, fmt.Errorf("nmcli %s: %w", strings.Join(redactedArgs, " "), err)
		}
		return out, fmt.Errorf("nmcli %s: %w: %s", strings.Join(redactedArgs, " "), err, msg)
	}
	return out, nil
}

// redactPassword returns the PSK from args, if any, and a copy of args with
// that value replaced by "***". This relies on an exact property of the two
// callers that ever put a secret in args, Connect and StartHotspot: when a
// PSK is present, it is always the last element of args, and it is always
// immediately preceded by the literal "password". Because that pair is
// always appended last, redaction anchors on the end of args rather than
// searching for the first element equal to "password" — the SSID is
// operator-typed and can itself be "password", and a left-to-right search
// would then redact the wrong element and let the real secret leak. Any
// future caller that puts a secret in args must preserve this "password",
// <psk> as its final two elements, or update this function.
func redactPassword(args []string) (secret string, redacted []string) {
	redacted = append([]string(nil), args...)
	if n := len(redacted); n >= 2 && redacted[n-2] == "password" {
		secret = redacted[n-1]
		redacted[n-1] = "***"
	}
	return secret, redacted
}

// execWaitDelay is how long a killed nmcli gets to let go of its output pipes
// before they are closed out from under it. See execRun; it is short because
// the process is already dead by the time it starts running.
const execWaitDelay = 2 * time.Second

func execRun(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	// WaitDelay is what actually makes tickDeadline a bound on the call, as
	// opposed to a bound on the process. CommandContext kills nmcli when the
	// context expires, but CombinedOutput then waits for the output pipe to
	// close, and the pipe is inherited: a descendant nmcli or NetworkManager
	// left holding stdout keeps Wait blocked long after the process it
	// belonged to is gone. Measured, a 1-second deadline returned after 30
	// seconds. WaitDelay caps that second wait — once the deadline has killed
	// the process, the pipes are closed after this delay whether anything is
	// still holding them or not, and the call returns with
	// exec.ErrWaitDelay.
	//
	// Everything in this package that argues a hung command cannot stop the
	// supervisor rests on this: Tick runs in Run's only goroutine, and a
	// device that stops supervising itself never raises the access point that
	// is its last way back.
	cmd.WaitDelay = execWaitDelay
	return cmd.CombinedOutput()
}

// errEmptyAPPassword is returned rather than raising an open access point.
// nmcli takes "password" as an optional argument, so omitting it produces an
// AP anyone within range can join — and joining this one reaches a dashboard
// that drives a school's public address system. An unreachable device costs a
// site visit; an open one lets a stranger sound an air raid alert or silence a
// real one. config.Validate deliberately still accepts an empty password as
// the "not generated yet" state, and netmgr.EnsureAPPassword fills it in at
// startup, so this guard is the backstop for a path that gets past both.
var errEmptyAPPassword = errors.New("refusing to raise an open access point: no password is set")

// Scan lists visible Wi-Fi networks, one per SSID, strongest signal first.
func (m NMCLI) Scan(ctx context.Context) ([]Network, error) {
	out, err := m.run(ctx, "-t", "-f", "SSID,SIGNAL,SECURITY", "device", "wifi", "list")
	if err != nil {
		return nil, err
	}
	return parseWifiList(out), nil
}

// parseWifiList reads `nmcli -t -f SSID,SIGNAL,SECURITY device wifi list`.
//
// SSID is the variable field here, and it comes first: SIGNAL and SECURITY
// are fixed in width and sit at the end, so they are taken from the end of
// the split and everything before them is rejoined with ":" back into the
// SSID. splitTerseFields already unescapes an SSID nmcli wrote with an
// escaped colon or backslash, in which case there is nothing left over to
// rejoin and this is a no-op; if nmcli instead emits a literal, unescaped
// colon inside the SSID, splitting produces extra fields and the rejoin
// restores it. Either way the result is the same. A hidden network reports
// an empty SSID; it cannot be shown in a list or picked from one, so it is
// dropped instead of rendered as a blank row. The same SSID can appear once
// per access point in range; a person sees one network, so only the
// strongest reading is kept.
func parseWifiList(out []byte) []Network {
	best := map[string]Network{}
	order := make([]string, 0)

	for _, line := range splitLines(out) {
		if line == "" {
			continue
		}
		fields := splitTerseFields(line)
		if len(fields) < 3 {
			continue
		}
		n := len(fields)
		ssid := strings.Join(fields[:n-2], ":")
		if ssid == "" {
			continue
		}
		signal, err := strconv.Atoi(fields[n-2])
		if err != nil {
			continue
		}
		secured := fields[n-1] != ""

		if existing, ok := best[ssid]; !ok {
			order = append(order, ssid)
			best[ssid] = Network{SSID: ssid, Signal: signal, Secured: secured}
		} else if signal > existing.Signal {
			best[ssid] = Network{SSID: ssid, Signal: signal, Secured: secured}
		}
	}

	nets := make([]Network, 0, len(order))
	for _, ssid := range order {
		nets = append(nets, best[ssid])
	}
	sort.SliceStable(nets, func(i, j int) bool { return nets[i].Signal > nets[j].Signal })
	return nets
}

// Status reports the current device state.
func (m NMCLI) Status(ctx context.Context) (Status, error) {
	out, err := m.run(ctx, "-t", "-f", "DEVICE,TYPE,STATE,CONNECTION", "device", "status")
	if err != nil {
		return Status{}, err
	}
	return parseStatus(out), nil
}

// nonUpstreamTypes are the nmcli device types that can report "connected"
// without that meaning the device has a way off the board. loopback is the
// one that matters: modern NetworkManager manages lo and reports it
// connected on every machine, so counting it would make every board look
// joined and none would ever raise its access point. The rest are local or
// virtual links — a peer-to-peer Wi-Fi association, a tunnel or bridge that
// is carried by some other device that is itself the real upstream.
//
// It is a list of exclusions rather than a list of allowed types on purpose:
// the failure that matters here is reading a genuinely connected board as
// disconnected and leaving a WPA2 access point up for good, so an unfamiliar
// device type counts as an upstream rather than being silently ignored.
var nonUpstreamTypes = map[string]bool{
	"loopback": true,
	"wifi-p2p": true,
	"tun":      true,
	"bridge":   true,
	"dummy":    true,
	"veth":     true,
}

// parseStatus reads `nmcli -t -f DEVICE,TYPE,STATE,CONNECTION device status`.
//
// CONNECTION is the variable field here, and unlike SSID above it trails:
// DEVICE, TYPE and STATE are fixed and come first, so they are taken from
// the front of the split and everything after them is rejoined with ":"
// back into the connection name. See parseWifiList and splitTerseFields for
// why that rejoin is needed either way nmcli emits a colon inside the name.
//
// A wifi device connected to hotspotConnID is serving the device's own
// access point, not joined to anything upstream; that yields HotspotUp true
// and Connected false. Telling those two apart by connection name is exactly
// why StartHotspot pins that name rather than accepting nmcli's default —
// without it this could not tell "the operator joined the school network"
// from "the hotspot is still up", which is the one distinction the
// supervisor built on top of this package needs. The comparison runs against
// the unescaped connection name, same as the name compared everywhere else
// in this package.
//
// Every other connected device that could be an upstream sets Connected,
// wifi or not. A board on a wall port in a PA closet is an ordinary install,
// and reading it as disconnected is what made it raise Spoloh-Setup after 45
// seconds and never lower it — a WPA2 access point broadcasting forever whose
// password reaches a dashboard that drives the school's speakers. SSID is
// still only ever a wifi device's network name: a wired profile is called
// something like "Wired connection 1", and putting that in front of an
// operator as an SSID would be a plain lie.
func parseStatus(out []byte) Status {
	var st Status
	for _, line := range splitLines(out) {
		if line == "" {
			continue
		}
		fields := splitTerseFields(line)
		if len(fields) < 4 {
			continue
		}
		devType, state := fields[1], fields[2]
		conn := strings.Join(fields[3:], ":")
		if state != "connected" {
			continue
		}
		if devType == "wifi" && conn == hotspotConnID {
			st.HotspotUp = true
			continue
		}
		if nonUpstreamTypes[devType] {
			continue
		}
		st.Connected = true
		if devType == "wifi" {
			st.SSID = conn
		}
	}
	return st
}

// splitLines splits nmcli's terse output into lines, tolerating a trailing
// newline (or none) without producing a spurious empty final element in the
// common case.
func splitLines(out []byte) []string {
	s := strings.TrimRight(string(out), "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// splitTerseFields splits one line of nmcli terse (`-t`) output into its
// colon-separated fields. nmcli escapes a literal colon or backslash inside
// a field's value as \: and \\, so this splits only on colons that are not
// preceded by a backslash, and unescapes each field as it goes — an escaped
// colon becomes a literal colon in the field, not a field boundary, and an
// escaped backslash collapses to one backslash. A backslash before any
// other byte is not an escape nmcli would have written, so it is kept
// literally rather than dropped: silently dropping it would let a
// malformed value decode into a string it is not.
//
// This does not by itself guarantee the right number of fields: if a caller
// somehow receives a value with an unescaped colon in it (nmcli not
// escaping it after all, or a future nmcli version behaving differently),
// splitting still produces more fields than expected. Callers with a
// variable-width field therefore take their fixed-width fields from
// whichever end they sit at and rejoin the rest; see parseWifiList and
// parseStatus. On correctly escaped input that rejoin is a no-op, because
// this function already returns the exact field count.
func splitTerseFields(line string) []string {
	var fields []string
	var cur strings.Builder
	// Bytes, not runes: ':' and '\\' are ASCII and cannot occur inside a
	// multi-byte UTF-8 sequence, so scanning bytes leaves every other
	// character untouched while making the one-byte lookahead below trivial.
	for i := 0; i < len(line); i++ {
		switch c := line[i]; c {
		case '\\':
			// nmcli escapes ':' and '\\' and nothing else. A backslash before
			// any other byte is therefore not an escape nmcli wrote, so it is
			// kept literally instead of being swallowed. Swallowing it would
			// let a malformed "spoloh\\-hotspot" decode to hotspotConnID and be
			// mistaken for this device's own access point.
			if i+1 < len(line) && (line[i+1] == ':' || line[i+1] == '\\') {
				cur.WriteByte(line[i+1])
				i++
				continue
			}
			cur.WriteByte(c)
		case ':':
			fields = append(fields, cur.String())
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
	}
	fields = append(fields, cur.String())
	return fields
}

// Connect joins the network ssid using psk. The SSID comes from a scan and
// the password from an operator; both are passed as separate argv elements
// to exec.CommandContext, never assembled into a shell string.
func (m NMCLI) Connect(ctx context.Context, ssid, psk string) error {
	args := []string{"device", "wifi", "connect", ssid}
	if psk != "" {
		args = append(args, "password", psk)
	}
	_, err := m.run(ctx, args...)
	return err
}

// StartHotspot raises a local access point with the given SSID and
// passphrase, under a fixed connection name so StopHotspot can find it again.
//
// The command carries no --wait 0, so nmcli activates the connection
// synchronously: a nil error means the interface is already on the hotspot
// profile by the time this call returns, not merely that activation was
// requested. netmgr.Supervisor's "raise it once, not every tick" behaviour
// depends on that — the very next Status call is expected to report
// HotspotUp true.
func (m NMCLI) StartHotspot(ctx context.Context, ssid, psk string) error {
	if psk == "" {
		return errEmptyAPPassword
	}
	args := []string{"device", "wifi", "hotspot", "con-name", hotspotConnID, "ssid", ssid, "password", psk}
	_, err := m.run(ctx, args...)
	return err
}

// StopHotspot brings down the connection profile StartHotspot created.
func (m NMCLI) StopHotspot(ctx context.Context) error {
	_, err := m.run(ctx, "connection", "down", hotspotConnID)
	return err
}
