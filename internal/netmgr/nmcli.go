package netmgr

import (
	"context"
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"
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
func (m NMCLI) run(ctx context.Context, args ...string) ([]byte, error) {
	run := m.Run
	if run == nil {
		run = execRun
	}
	out, err := run(ctx, m.bin(), args...)
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			return out, fmt.Errorf("nmcli %s: %w", strings.Join(args, " "), err)
		}
		return out, fmt.Errorf("nmcli %s: %w: %s", strings.Join(args, " "), err, msg)
	}
	return out, nil
}

func execRun(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

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
// Fields are colon-separated and an SSID may itself contain a colon, so the
// split is bounded to the field count rather than unbounded. A hidden
// network reports an empty SSID; it cannot be shown in a list or picked from
// one, so it is dropped instead of rendered as a blank row. The same SSID
// can appear once per access point in range; a person sees one network, so
// only the strongest reading is kept.
func parseWifiList(out []byte) []Network {
	best := map[string]Network{}
	order := make([]string, 0)

	for _, line := range splitLines(out) {
		if line == "" {
			continue
		}
		fields := strings.SplitN(line, ":", 3)
		if len(fields) < 3 {
			continue
		}
		ssid := fields[0]
		if ssid == "" {
			continue
		}
		signal, err := strconv.Atoi(fields[1])
		if err != nil {
			continue
		}
		secured := fields[2] != ""

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

// parseStatus reads `nmcli -t -f DEVICE,TYPE,STATE,CONNECTION device status`.
//
// A wifi device connected to hotspotConnID is serving the device's own
// access point, not joined to anything upstream; that yields HotspotUp true
// and Connected false. Any other connected wifi device is on an upstream
// network: Connected true, SSID set, HotspotUp false. Telling these two
// apart by connection name is exactly why StartHotspot pins that name rather
// than accepting nmcli's default — without it this line could not tell "the
// operator joined the school network" from "the hotspot is still up", which
// is the one distinction the supervisor built on top of this package needs.
func parseStatus(out []byte) Status {
	var st Status
	for _, line := range splitLines(out) {
		if line == "" {
			continue
		}
		fields := strings.SplitN(line, ":", 4)
		if len(fields) < 4 {
			continue
		}
		devType, state, conn := fields[1], fields[2], fields[3]
		if devType != "wifi" || state != "connected" {
			continue
		}
		if conn == hotspotConnID {
			st.HotspotUp = true
			continue
		}
		st.Connected = true
		st.SSID = conn
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
func (m NMCLI) StartHotspot(ctx context.Context, ssid, psk string) error {
	args := []string{"device", "wifi", "hotspot", "con-name", hotspotConnID, "ssid", ssid}
	if psk != "" {
		args = append(args, "password", psk)
	}
	_, err := m.run(ctx, args...)
	return err
}

// StopHotspot brings down the connection profile StartHotspot created.
func (m NMCLI) StopHotspot(ctx context.Context) error {
	_, err := m.run(ctx, "connection", "down", hotspotConnID)
	return err
}
