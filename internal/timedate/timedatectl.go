package timedate

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/b-ovsepian/spoloh/internal/atomicfile"
	"github.com/b-ovsepian/spoloh/internal/config"
)

// RunFunc executes a command and returns its combined output. Injected so
// Timedatectl can be tested without a systemd host, matching the pattern
// internal/clock uses for the read side of this same tool and internal/netmgr
// uses for nmcli.
type RunFunc func(ctx context.Context, name string, args ...string) ([]byte, error)

// execTimeout caps one timedatectl or systemctl invocation. The person
// waiting is looking at a phone: a page that says the call failed is better
// than one that never comes back. It is longer than internal/clock's 3s
// because these calls go through polkit and a write, not a property read.
const execTimeout = 10 * time.Second

// execWaitDelay is how long a killed child gets to let go of its output pipes
// before they are closed out from under it; see netmgr's execRun for the
// measurement this number comes from.
const execWaitDelay = 2 * time.Second

// timesyncUnit is the oneshot unit that carries the configured NTP server
// into /etc/systemd/timesyncd.conf.d/, the directory this daemon cannot
// write. deploy/50-spoloh-timedate.rules grants manage-units for this one
// unit name and no other, so this constant and that rule have to agree:
// change one and the daemon's start is denied by polkit.
const timesyncUnit = "spoloh-timesync.service"

// defaultServerFile is where the daemon leaves the chosen NTP server for
// that unit to read. It is inside /etc/spoloh, the one directory outside
// StateDirectory that spoloh.service grants this user write access to
// (ReadWritePaths=-/etc/spoloh), and it holds the bare server and nothing
// else — the [Time] stanza itself is written by root, from a shape root
// controls. See ApplyNTPServer.
const defaultServerFile = "/etc/spoloh/ntp-server"

// Timedatectl drives systemd's time settings through the timedatectl and
// systemctl binaries. It holds no state of its own beyond its configuration,
// so a value is fine to pass around.
type Timedatectl struct {
	// Bin is the timedatectl executable to run. Empty means "timedatectl" on
	// PATH.
	Bin string
	// Systemctl is the systemctl executable to run. Empty means "systemctl"
	// on PATH.
	Systemctl string
	// ServerFile is where ApplyNTPServer writes the chosen NTP server for
	// the oneshot unit to read. Empty means defaultServerFile.
	ServerFile string
	// Run replaces the exec of both binaries. Nil means the real thing.
	Run RunFunc
}

var _ Manager = Timedatectl{}

func (t Timedatectl) bin() string {
	if t.Bin != "" {
		return t.Bin
	}
	return "timedatectl"
}

func (t Timedatectl) systemctl() string {
	if t.Systemctl != "" {
		return t.Systemctl
	}
	return "systemctl"
}

func (t Timedatectl) serverFile() string {
	if t.ServerFile != "" {
		return t.ServerFile
	}
	return defaultServerFile
}

// run executes name with args under execTimeout and returns its output. On
// failure the error carries the command's own combined output: polkit
// denials, "Failed to set time: Automatic time synchronization is enabled"
// and a missing binary all look identical without it, and that message is
// the only thing that tells an operator which of them happened. Nothing
// passed to these commands is a secret, so unlike netmgr's equivalent there
// is nothing here to redact.
func (t Timedatectl) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	run := t.Run
	if run == nil {
		run = execRun
	}
	callCtx, cancel := context.WithTimeout(ctx, execTimeout)
	defer cancel()

	out, err := run(callCtx, name, args...)
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			return out, fmt.Errorf("%s %s: %w", filepath.Base(name), strings.Join(args, " "), err)
		}
		return out, fmt.Errorf("%s %s: %w: %s", filepath.Base(name), strings.Join(args, " "), err, msg)
	}
	return out, nil
}

func execRun(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	// See netmgr.execRun: without WaitDelay the context deadline bounds the
	// process, not the call, because CombinedOutput goes on waiting for an
	// inherited pipe a descendant still holds.
	cmd.WaitDelay = execWaitDelay
	return cmd.CombinedOutput()
}

// Show reads `timedatectl show`, whose Key=Value output is stable and
// unlocalised — unlike `timedatectl status`, which is prose translated into
// the host's language and would parse differently on a board somebody set to
// Ukrainian.
func (t Timedatectl) Show(ctx context.Context) (State, error) {
	out, err := t.run(ctx, t.bin(), "show")
	if err != nil {
		return State{}, err
	}
	return parseShow(out), nil
}

// parseShow reads `timedatectl show`'s Key=Value lines.
//
// Unknown keys are ignored rather than rejected: systemd adds properties
// between versions (TimeUSec and RTCTimeUSec are not read here, and
// NTPSynchronized itself was not always present), and a parser that failed
// on one it did not recognise would turn a systemd upgrade into a dashboard
// that reports nothing about the clock. A line with no "=" is skipped for
// the same reason. Only the first occurrence of a key counts, so a repeated
// key cannot quietly overwrite a value already read.
func parseShow(out []byte) State {
	var st State
	seen := map[string]bool{}
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok || seen[key] {
			continue
		}
		seen[key] = true
		switch key {
		case "Timezone":
			st.Timezone = value
		case "LocalRTC":
			st.LocalRTC = value == "yes"
		case "CanNTP":
			st.CanNTP = value == "yes"
		case "NTP":
			st.NTP = value == "yes"
		case "NTPSynchronized":
			st.NTPSynchronized = value == "yes"
		}
	}
	return st
}

// Zones lists the host's time zones. This is `timedatectl list-timezones`
// rather than a walk of /usr/share/zoneinfo: it is the same data, read
// through the seam every other call in this package already goes through,
// and it needs no privilege — listing zones is not polkit-gated, only
// setting one is.
func (t Timedatectl) Zones(ctx context.Context) ([]string, error) {
	// --no-pager because timedatectl pipes this list through a pager when it
	// thinks it is talking to a terminal, and a daemon reading a pager's
	// output gets control sequences or a hang.
	out, err := t.run(ctx, t.bin(), "list-timezones", "--no-pager")
	if err != nil {
		return nil, err
	}
	var zones []string
	for _, line := range strings.Split(string(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			zones = append(zones, line)
		}
	}
	return zones, nil
}

// SetTimezone runs `timedatectl set-timezone`. Polkit-gated:
// org.freedesktop.timedate1.set-timezone.
func (t Timedatectl) SetTimezone(ctx context.Context, tz string) error {
	_, err := t.run(ctx, t.bin(), "set-timezone", tz)
	return err
}

// SetNTP runs `timedatectl set-ntp`. Polkit-gated:
// org.freedesktop.timedate1.set-ntp.
func (t Timedatectl) SetNTP(ctx context.Context, on bool) error {
	value := "false"
	if on {
		value = "true"
	}
	_, err := t.run(ctx, t.bin(), "set-ntp", value)
	return err
}

// SetTime runs `timedatectl set-time`. Polkit-gated:
// org.freedesktop.timedate1.set-time. systemd refuses this outright while
// NTP is on; callers are expected to have said so already rather than
// letting an operator submit it and read systemd's English refusal.
func (t Timedatectl) SetTime(ctx context.Context, stamp string) error {
	_, err := t.run(ctx, t.bin(), "set-time", stamp)
	return err
}

// ApplyNTPServer records server and asks the system to adopt it.
//
// This is deliberately not one privileged step but two, because the file
// that has to change is root-owned and this daemon is not root:
//
//  1. The daemon writes the bare server — nothing else, no stanza, no
//     syntax — to ServerFile, inside the one directory spoloh.service lets
//     it write.
//  2. `systemctl start spoloh-timesync.service` runs a root oneshot that
//     turns that one token into /etc/systemd/timesyncd.conf.d/spoloh.conf
//     and restarts systemd-timesyncd.
//
// Splitting it that way means the shape of a root-owned config file is
// written by root, from a template in the unit, and the unprivileged side
// contributes a single validated token to it. An empty server clears the
// file, which is the unit's signal to remove the drop-in and fall back to
// whatever the distribution configures.
//
// The value is re-validated here even though config.Validate has already
// rejected anything with a newline, a quote or an "=" in it: this writes a
// file that root parses, and a check at the boundary that does the writing
// is what makes that true regardless of how the value arrived.
func (t Timedatectl) ApplyNTPServer(ctx context.Context, server string) error {
	if err := config.ValidateNTPServer(server); err != nil {
		return err
	}
	body := ""
	if server != "" {
		body = server + "\n"
	}
	// 0644: the unit reads this as root, and it is a hostname, not a secret.
	if err := atomicfile.Write(t.serverFile(), []byte(body), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", t.serverFile(), err)
	}
	_, err := t.run(ctx, t.systemctl(), "start", timesyncUnit)
	return err
}
