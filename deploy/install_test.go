package deploy

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The installer is shell, runs as root and talks to apt, systemd and the
// network — none of which a test may do. So it runs here against a directory
// of fakes on PATH and a local download directory, which is enough to prove
// the parts that actually go wrong: the architecture mapping, the checksum
// gate, which files land where, and that the access point password is read
// back and printed.
//
// newInstallerCmd builds the *exec.Cmd; runInstaller (below) is the common
// blocking case, and TestInstallerExitsOnSIGTERM below needs the unstarted
// command itself so it can signal the process mid-run.
func newInstallerCmd(t *testing.T, downloads, uname string, extraEnv ...string) (cmd *exec.Cmd, logPath, prefix string) {
	t.Helper()
	root := t.TempDir()
	fakeBin := filepath.Join(root, "bin")
	if err := os.MkdirAll(fakeBin, 0o755); err != nil {
		t.Fatal(err)
	}

	// Each fake records that it ran and succeeds. curl copies out of the
	// local download directory instead of reaching the network.
	fakes := map[string]string{
		"uname":            "#!/bin/sh\nif [ \"$1\" = -m ]; then echo " + uname + "; else echo Linux; fi\n",
		"id":               "#!/bin/sh\necho 0\n",
		"apt-get":          "#!/bin/sh\necho \"apt-get $*\" >> \"$SPOLOH_TEST_LOG\"\n",
		"systemd-sysusers": "#!/bin/sh\necho \"systemd-sysusers $*\" >> \"$SPOLOH_TEST_LOG\"\n",
		"systemd-tmpfiles": "#!/bin/sh\necho \"systemd-tmpfiles $*\" >> \"$SPOLOH_TEST_LOG\"\n",
		"systemctl":        "#!/bin/sh\necho \"systemctl $*\" >> \"$SPOLOH_TEST_LOG\"\n",
		// set-hostname is recorded, and the hostname fake below reads it
		// back, because the script asks for the hostname again after the
		// rename to repair /etc/hosts. A fake that kept answering with the
		// old name would make that repair untestable and, worse, make a
		// wrong answer look right.
		"hostnamectl": "#!/bin/sh\necho \"hostnamectl $*\" >> \"$SPOLOH_TEST_LOG\"\nif [ \"$1\" = set-hostname ]; then echo \"$2\" > \"$SPOLOH_TEST_HOSTNAME_FILE\"; fi\n",
		// The non-`-I` branch defaults to a hostname that is not "spoloh",
		// so the every-test-but-one path exercises the rename; overridden via
		// SPOLOH_TEST_HOSTNAME by the idempotence test below.
		// SPOLOH_TEST_HOSTNAME_FAILS and _EMPTY are checked before the
		// recorded name, so they survive the rename: a board where
		// `hostname` is missing or answers nothing is a board where it
		// does so for the whole run.
		"hostname": "#!/bin/sh\n" +
			"if [ \"$1\" = -I ]; then echo 192.168.1.50\n" +
			"elif [ -n \"${SPOLOH_TEST_HOSTNAME_FAILS:-}\" ]; then exit 127\n" +
			"elif [ -n \"${SPOLOH_TEST_HOSTNAME_EMPTY:-}\" ]; then echo \"\"\n" +
			"elif [ -f \"$SPOLOH_TEST_HOSTNAME_FILE\" ]; then cat \"$SPOLOH_TEST_HOSTNAME_FILE\"\n" +
			"else echo \"${SPOLOH_TEST_HOSTNAME:-raspberrypi}\"\n" +
			"fi\n",
		"curl": "#!/bin/sh\nwhile [ $# -gt 0 ]; do case $1 in -o) out=$2; shift 2;; -*) shift;; *) url=$1; shift;; esac; done\ncp \"$SPOLOH_TEST_DOWNLOADS/$(basename \"$url\")\" \"$out\"\n",
		// install -D and sha256sum are GNU coreutils; a developer's macOS has
		// neither. These shims are faithful to the two behaviours the script
		// depends on, so the checksum gate is still genuinely exercised rather
		// than stubbed out. One behaviour this shim does *not* reproduce: real
		// install(1) unlinks the destination before writing, so it would not
		// hit ETXTBSY against a currently-running binary; this shim's cp would.
		// Irrelevant to install.sh, which only ever runs before the service
		// exists, but worth keeping honest since the shim is a stand-in.
		"install": "#!/bin/sh\nmode=0644\nwhile [ $# -gt 2 ]; do case $1 in -m) mode=$2; shift 2;; *) shift;; esac; done\nmkdir -p \"$(dirname \"$2\")\"\ncp \"$1\" \"$2\"\nchmod \"$mode\" \"$2\"\n",
	}
	// Only shimmed when the real thing is absent, so on Linux the script is
	// checked against the binary it will actually run against.
	if _, err := exec.LookPath("sha256sum"); err != nil {
		fakes["sha256sum"] = "#!/bin/sh\nexec shasum -a 256 \"$@\"\n"
	}
	for name, body := range fakes {
		if err := os.WriteFile(filepath.Join(fakeBin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	logPath = filepath.Join(root, "log")
	prefix = filepath.Join(root, "prefix")
	cmd = exec.Command("sh", "install.sh")
	cmd.Env = append(os.Environ(),
		"PATH="+fakeBin+":"+os.Getenv("PATH"),
		"SPOLOH_PREFIX="+prefix,
		"SPOLOH_BASE_URL=https://example.invalid/releases",
		"SPOLOH_TEST_LOG="+logPath,
		"SPOLOH_TEST_DOWNLOADS="+downloads,
		"SPOLOH_TEST_HOSTNAME_FILE="+filepath.Join(root, "hostname"),
		// No config.json will ever appear in these runs, and a test may not
		// sleep on a real clock. The password test overrides this; the SIGTERM
		// test raises it just enough to still be inside the wait loop when the
		// signal is sent.
		"SPOLOH_AP_WAIT=0",
	)
	cmd.Env = append(cmd.Env, extraEnv...)
	return cmd, logPath, prefix
}

func runInstaller(t *testing.T, downloads, uname string, extraEnv ...string) (string, error) {
	t.Helper()
	cmd, logPath, prefix := newInstallerCmd(t, downloads, uname, extraEnv...)
	out, err := cmd.CombinedOutput()
	t.Logf("installer output:\n%s", out)
	return string(out) + "\n--- commands ---\n" + readFile(logPath) + "\n--- prefix ---\n" + tree(t, prefix), err
}

func readFile(p string) string { b, _ := os.ReadFile(p); return string(b) }

func tree(t *testing.T, root string) string {
	t.Helper()
	var b strings.Builder
	filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			rel, _ := filepath.Rel(root, p)
			fmt.Fprintf(&b, "%s\n", rel)
		}
		return nil
	})
	return b.String()
}

// downloadDir builds what a release looks like on disk, with correct
// checksums unless corrupt names a file to spoil.
func downloadDir(t *testing.T, corrupt string) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"spoloh-linux-armv6":             "binary armv6",
		"spoloh-linux-armv7":             "binary armv7",
		"spoloh-linux-arm64":             "binary arm64",
		"spoloh.service":                 "[Unit]\n",
		"spoloh.sysusers":                "u spoloh\n",
		"spoloh.tmpfiles":                "d /etc/spoloh\n",
		"50-spoloh-networkmanager.rules": "polkit.addRule(function(){});\n",
		"spoloh-timesync.service":        "[Unit]\n",
		"50-spoloh-timedate.rules":       "polkit.addRule(function(){});\n",
	}
	var sums strings.Builder
	for name, body := range files {
		h := sha256.Sum256([]byte(body))
		fmt.Fprintf(&sums, "%s  %s\n", hex.EncodeToString(h[:]), name)
		if name == corrupt {
			body = "tampered"
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "SHA256SUMS"), []byte(sums.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestInstallerPlacesEveryFileAndStartsTheUnit(t *testing.T) {
	report, err := runInstaller(t, downloadDir(t, ""), "armv7l")
	if err != nil {
		t.Fatalf("installer failed: %v\n%s", err, report)
	}
	for _, want := range []string{
		"usr/local/bin/spoloh",
		"etc/systemd/system/spoloh.service",
		"usr/lib/sysusers.d/spoloh.conf",
		"usr/lib/tmpfiles.d/spoloh.conf",
		"etc/polkit-1/rules.d/50-spoloh-networkmanager.rules",
		// The /time page cannot work without either of these: the polkit
		// rule is what lets an unprivileged daemon call timedatectl at all,
		// and the oneshot is the only thing that can write the NTP server
		// into root-owned /etc/systemd/timesyncd.conf.d/.
		"etc/systemd/system/spoloh-timesync.service",
		"etc/polkit-1/rules.d/50-spoloh-timedate.rules",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("%s was not installed\n%s", want, report)
		}
	}
	for _, want := range []string{
		"systemd-tmpfiles --create", // without this the daemon crash-loops on a read-only /etc
		"systemctl enable spoloh",
		// enable alone is not enough: on a re-run against an already-active
		// unit `enable --now` is a no-op, so the replaced binary on disk
		// would never become the running process - and `spoloh update`
		// execs the on-disk binary to learn its version, so the device
		// would then report itself up to date forever while running the old
		// code. restart is what makes a re-install actually take effect.
		"systemctl restart spoloh",
		"hostnamectl set-hostname spoloh",
		// avahi is already publishing raspberrypi.local on a stock image
		// and is not reliably notified of a hostname change, so without
		// this the banner prints a spoloh.local that does not resolve.
		"systemctl try-restart avahi-daemon",
		"apt-get install",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("installer never ran %q\n%s", want, report)
		}
	}
}

func TestInstallerRefusesACorruptDownloadAndInstallsNothing(t *testing.T) {
	report, err := runInstaller(t, downloadDir(t, "spoloh-linux-armv7"), "armv7l")
	if err == nil {
		t.Fatalf("installer accepted a corrupt binary\n%s", report)
	}
	if !strings.Contains(report, "checksum mismatch") {
		t.Errorf("failure did not name the checksum\n%s", report)
	}
	if strings.Contains(report, "usr/local/bin/spoloh") {
		t.Errorf("a corrupt download was installed anyway\n%s", report)
	}
	if strings.Contains(report, "systemctl enable") {
		t.Errorf("the unit was started despite the refusal\n%s", report)
	}
	// The script's checksum loop runs entirely before the apt-get step; this
	// pins that ordering down instead of relying on it as an accident of
	// where the die happens to land in the file.
	if strings.Contains(report, "apt-get") {
		t.Errorf("packages were installed despite the checksum failure\n%s", report)
	}
}

func TestInstallerMapsEachBoardToItsArtifact(t *testing.T) {
	for machine, artifact := range map[string]string{
		"armv6l":  "linux-armv6",
		"armv7l":  "linux-armv7",
		"aarch64": "linux-arm64",
	} {
		report, err := runInstaller(t, downloadDir(t, ""), machine)
		if err != nil {
			t.Fatalf("%s: %v\n%s", machine, err, report)
		}
		if !strings.Contains(report, "-> "+artifact) {
			t.Errorf("%s chose the wrong artifact\n%s", machine, report)
		}
	}
}

func TestInstallerRefusesAnUnsupportedBoard(t *testing.T) {
	report, err := runInstaller(t, downloadDir(t, ""), "x86_64")
	if err == nil {
		t.Fatalf("installer ran on an unsupported board\n%s", report)
	}
	if !strings.Contains(report, "unsupported architecture") {
		t.Errorf("failure did not explain why\n%s", report)
	}
}

func TestInstallerPrintsTheAccessPointPassword(t *testing.T) {
	// The daemon writes this on first start. Until the installer prints it,
	// it exists only in the journal, which a volunteer with no monitor
	// cannot reach — the first-run dead end this closes.
	dir := t.TempDir()
	cfg := filepath.Join(dir, "etc", "spoloh")
	if err := os.MkdirAll(cfg, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg, "config.json"),
		[]byte(`{"network":{"ap_ssid":"Spoloh-Setup","ap_password":"sp0lohTest42"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	// Point PREFIX at the directory that already holds a config, the way it
	// looks a second after the unit starts.
	report, err := runInstaller(t, downloadDir(t, ""), "armv7l", "SPOLOH_PREFIX="+dir)
	if err != nil {
		t.Fatalf("installer failed: %v\n%s", err, report)
	}
	if !strings.Contains(report, "sp0lohTest42") {
		t.Errorf("the access point password was never printed\n%s", report)
	}
	if !strings.Contains(report, "http://spoloh.local/") {
		t.Errorf("the dashboard address was never printed\n%s", report)
	}
	// The SSID and the password on their own name a network to join and no
	// address to open once you are on it. 10.42.0.1 is the gateway
	// NetworkManager's shared mode hands out, which is what
	// `nmcli device wifi hotspot` raises.
	if !strings.Contains(report, "http://10.42.0.1/") {
		t.Errorf("the access point's address was never printed\n%s", report)
	}
}

// The 127.0.1.1 line is what stops sudo printing "unable to resolve host"
// after the rename. Harmless to the daemon, alarming to a volunteer who has
// just set the device up and cannot tell a cosmetic warning from a real one.
//
// The repair runs on every install, not only on the one that does the
// renaming: a device installed by an earlier version of this script is
// already called spoloh and still has raspberrypi in its hosts file, because
// that version never touched it. A repair that only ran on the renaming pass
// would skip exactly the devices that have been in the field longest.
func TestInstallerKeepsTheLoopbackHostEntryOnTheHostname(t *testing.T) {
	const (
		renamed  = "127.0.0.1\tlocalhost\n127.0.1.1\traspberrypi\n::1\tip6-localhost\n"
		repaired = "127.0.0.1\tlocalhost\n127.0.1.1\tspoloh\n::1\tip6-localhost\n"
	)
	for name, tc := range map[string]struct {
		hosts    string
		hostname string // SPOLOH_TEST_HOSTNAME, "" for the default raspberrypi
		env      string // an extra fake-hostname knob, "" for none
		want     string
		changed  bool
	}{
		// The ordinary first install: this run renames the board, and the
		// hosts entry has to follow it.
		"a device being renamed now": {hosts: renamed, want: repaired, changed: true},
		// The case the earlier, rename-only version of this left behind
		// forever.
		"a device already named spoloh whose hosts file still says otherwise": {
			hosts: renamed, hostname: "spoloh", want: repaired, changed: true,
		},
		// Rewriting the line to the value it already holds must not be a
		// rewrite at all.
		"a device whose hosts file is already right": {
			hosts: repaired, hostname: "spoloh", want: repaired,
		},
		// Aliases after the hostname are somebody's addition and the line
		// already names the host, so there is nothing to repair.
		"a hosts entry that names the host and then an alias": {
			hosts:    "127.0.1.1\tspoloh\tpa-system\n",
			hostname: "spoloh",
			want:     "127.0.1.1\tspoloh\tpa-system\n",
		},
		// No 127.0.1.1 line at all is a deliberate configuration, and not
		// ours to invent one in.
		"a hosts file with no loopback host entry": {
			hosts: "127.0.0.1\tlocalhost\n::1\tip6-localhost\n",
			want:  "127.0.0.1\tlocalhost\n::1\tip6-localhost\n",
		},
		// Everything after the first name on the line is somebody's
		// addition and has to survive the repair. Only the name is
		// replaced.
		"aliases on the entry being repaired": {
			hosts:   "127.0.0.1\tlocalhost\n127.0.1.1\traspberrypi\tpa-system\tbell\n",
			want:    "127.0.0.1\tlocalhost\n127.0.1.1\tspoloh\tpa-system\tbell\n",
			changed: true,
		},
		// Two of them is an unusual file somebody built on purpose.
		// Which one is "the" host entry is not ours to decide.
		"two loopback host entries": {
			hosts: "127.0.1.1\traspberrypi\n127.0.1.1\told-name\n",
			want:  "127.0.1.1\traspberrypi\n127.0.1.1\told-name\n",
		},
		// A hostname of "" would write `127.0.1.1` with no name at all,
		// destroying the working entry it was repairing.
		"a hostname that answers nothing": {
			hosts: renamed, env: "SPOLOH_TEST_HOSTNAME_EMPTY=1", want: renamed,
		},
		// The blocker this table's last two cases exist for: `hostname`
		// is called in a bare assignment now, so a board without it used
		// to kill the script under `set -eu` after every file was placed
		// and before the unit was ever enabled — a half-installed device
		// with no banner saying so.
		"a hostname that fails": {
			hosts: renamed, env: "SPOLOH_TEST_HOSTNAME_FAILS=1", want: renamed,
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.MkdirAll(filepath.Join(dir, "etc"), 0o755); err != nil {
				t.Fatal(err)
			}
			hosts := filepath.Join(dir, "etc", "hosts")
			if err := os.WriteFile(hosts, []byte(tc.hosts), 0o644); err != nil {
				t.Fatal(err)
			}
			env := []string{"SPOLOH_PREFIX=" + dir}
			if tc.hostname != "" {
				env = append(env, "SPOLOH_TEST_HOSTNAME="+tc.hostname)
			}
			if tc.env != "" {
				env = append(env, tc.env)
			}

			report, err := runInstaller(t, downloadDir(t, ""), "armv7l", env...)
			if err != nil {
				t.Fatalf("installer failed: %v\n%s", err, report)
			}

			if got := readFile(hosts); got != tc.want {
				t.Errorf("/etc/hosts is\n%q\nwant\n%q\n%s", got, tc.want, report)
			}
			// Asserted in every case, because this repair sits between
			// placing the files and starting the unit: anything here
			// that stops the script leaves a device with the binary on
			// disk, no unit enabled, and no banner saying so. That is
			// the half-installed device the whole script is arranged to
			// prevent, and `hostname` failing on an unusual board must
			// not be able to cause it.
			for _, want := range []string{"systemctl restart spoloh", "Spoloh is installed."} {
				if !strings.Contains(report, want) {
					t.Errorf("the install did not finish: no %q\n%s", want, report)
				}
			}
			// A run that changes nothing must also say nothing: the line
			// is what tells a reader of the transcript that the file was
			// rewritten, so a silent rewrite and a noisy no-op are both
			// wrong.
			if said := strings.Contains(report, "pointing the 127.0.1.1 entry"); said != tc.changed {
				t.Errorf("installer reported rewriting the entry = %v, want %v\n%s", said, tc.changed, report)
			}
			// Copied over the existing file rather than renamed into
			// place, so its mode is the one it already had rather than
			// whatever this shell's umask allows.
			info, err := os.Stat(hosts)
			if err != nil {
				t.Fatal(err)
			}
			if mode := info.Mode().Perm(); mode != 0o644 {
				t.Errorf("/etc/hosts is %v, want -rw-r--r--", mode)
			}
		})
	}
}

func TestInstallerSkipsHostnameChangeWhenAlreadySpoloh(t *testing.T) {
	// The brief asks specifically about idempotence here: a device already
	// named spoloh (a re-run, or a second install attempt) must not call
	// hostnamectl again.
	report, err := runInstaller(t, downloadDir(t, ""), "armv7l", "SPOLOH_TEST_HOSTNAME=spoloh")
	if err != nil {
		t.Fatalf("installer failed: %v\n%s", err, report)
	}
	if strings.Contains(report, "hostnamectl") {
		t.Errorf("hostnamectl ran even though the hostname was already spoloh\n%s", report)
	}
	if strings.Contains(report, "try-restart avahi-daemon") {
		t.Errorf("avahi was restarted even though the hostname did not change\n%s", report)
	}
}

// TestInstallerExitsOnSIGTERM proves the fix for the trap bug: a single
// `trap ... EXIT INT TERM` runs the handler and then resumes at the next
// statement under POSIX, rather than terminating the process, so a
// volunteer's Ctrl-C (or an init system's SIGTERM) during the run would
// delete the scratch directory out from under the script and let it
// continue to completion anyway — packages installed, binary not. The fix
// gives INT and TERM their own handlers that call exit explicitly.
func TestInstallerExitsOnSIGTERM(t *testing.T) {
	downloads := downloadDir(t, "")
	// High enough that the wait loop is still sleeping when the signal
	// arrives, but low enough that even the unfixed script (which ignores
	// the signal and runs to completion) finishes well inside the test's own
	// timeout, rather than this test hanging on a regression.
	cmd, _, prefix := newInstallerCmd(t, downloads, "armv7l", "SPOLOH_AP_WAIT=5")
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start installer: %v", err)
	}

	// Poll for the installed binary rather than sleeping a guessed interval:
	// step 5 (file placement) runs before the access-point wait loop, so its
	// appearance means the script has moved on to systemd/systemctl and is
	// at or about to enter the loop the signal needs to interrupt.
	binPath := filepath.Join(prefix, "usr", "local", "bin", "spoloh")
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(binPath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			t.Fatalf("installer never reached the point of installing the binary\n%s", out.String())
		}
		time.Sleep(5 * time.Millisecond)
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal installer: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		exitErr, ok := err.(*exec.ExitError)
		if err == nil || !ok {
			t.Fatalf("installer did not fail after SIGTERM (err=%v) — the signal was not honored\n%s", err, out.String())
		}
		// 143 = 128 + SIGTERM(15), the trap's own convention (see
		// install.sh), which also rules out the process merely being killed
		// by the OS after the test's own bounded wait rather than exiting on
		// its own via the trap.
		if code := exitErr.ExitCode(); code != 143 {
			t.Errorf("installer exited %d after SIGTERM, want 143\n%s", code, out.String())
		}
	case <-time.After(3 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatalf("installer did not exit within 3s of SIGTERM — it survived the signal and kept running\n%s", out.String())
	}
}

// TestATruncatedScriptInstallsNothing is the `curl | sudo sh` failure the
// straight-line version of this script had no defence against: sh executes
// whatever bytes arrive, so a connection dropped mid-download ran the first
// half of the install and exited 0. Cut just before the file-placement step
// that is exactly four apt packages installed, no files placed, success
// reported — the half-installed device section 3's own comment says the
// script exists to prevent, reached through the delivery method the README
// prescribes.
//
// The fix is structural rather than a check: with the body inside main() and
// `main "$@"` as the last line of the file, a truncated copy defines a
// function and runs nothing.
func TestATruncatedScriptInstallsNothing(t *testing.T) {
	script, err := os.ReadFile("install.sh")
	if err != nil {
		t.Fatal(err)
	}
	// Cut at a marker rather than a line number so this keeps testing the
	// same thing as the script grows: everything before step 5, which is
	// where the first file would be placed.
	const marker = "# 5. Files."
	lines := strings.SplitAfter(string(script), "\n")
	cut := -1
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), marker) {
			cut = i
			break
		}
	}
	if cut < 0 {
		t.Fatalf("install.sh no longer contains %q, so this test cannot find the truncation point", marker)
	}
	truncated := strings.Join(lines[:cut], "")
	// The whole point is a cut that lands after the package step: a cut
	// before it would prove nothing, because there would be nothing
	// destructive above the cut to run.
	if !strings.Contains(truncated, "apt-get install") {
		t.Fatalf("the truncation point is above the package step, so this test proves nothing")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "install.sh")
	if err := os.WriteFile(path, []byte(truncated), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd, logPath, prefix := newInstallerCmd(t, downloadDir(t, ""), "armv7l")
	cmd.Args = []string{"sh", path}
	out, err := cmd.CombinedOutput()
	report := string(out) + "\n--- commands ---\n" + readFile(logPath) + "\n--- prefix ---\n" + tree(t, prefix)
	t.Logf("truncated installer:\n%s", report)

	if err == nil {
		t.Errorf("a truncated script exited 0, so a dropped connection reports a successful install\n%s", report)
	}
	if placed := tree(t, prefix); placed != "" {
		t.Errorf("a truncated script placed files:\n%s\n%s", placed, report)
	}
	if strings.Contains(report, "apt-get") {
		t.Errorf("a truncated script ran apt-get, so it changed the system before stopping\n%s", report)
	}
}

// The deployment files cross a seam nothing else checks. The Makefile names
// them twice — once to copy them into dist/, once to hash them into
// SHA256SUMS — and install.sh names them a third time, in another language,
// to download and verify them. cmd/spoloh/arch_test.go pins the three binary
// names across the same seam; these four had nothing.
//
// Renaming one of them in the Makefile and in deploy/ but not in install.sh
// leaves `go vet`, `gofmt`, `go test` and `make dist` all clean, and every
// install in the field 404s against the published release — discovered by a
// volunteer at a site, which is the one place a bad install cannot be
// recovered from remotely.
//
// This lives beside the installer rather than in cmd/spoloh because
// install.sh is the file with the least machine-checkable claim in it: the
// Makefile at least fails loudly when it cannot copy a file that does not
// exist, while install.sh's FILES list is a string nothing validates until a
// device runs it.
func TestTheMakefileAndTheInstallerAgreeOnTheDeploymentFilenames(t *testing.T) {
	makefile, err := os.ReadFile("../Makefile")
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	script, err := os.ReadFile("install.sh")
	if err != nil {
		t.Fatalf("read install.sh: %v", err)
	}

	// dist/ is populated by `cp deploy/<file> ... dist/`.
	cp := regexp.MustCompile(`(?m)^\s*cp ((?:deploy/\S+ )+)dist/\s*$`).FindSubmatch(makefile)
	if cp == nil {
		t.Fatal("the Makefile no longer has a `cp deploy/... dist/` line; nothing copies the deployment files into the release")
	}
	// SHA256SUMS covers everything the release publishes a checksum for.
	sum := regexp.MustCompile(`(?m)^\s*cd dist && sha256sum (.+) > SHA256SUMS\s*$`).FindSubmatch(makefile)
	if sum == nil {
		t.Fatal("the Makefile no longer has a `sha256sum ... > SHA256SUMS` line; the release publishes no checksums")
	}
	// install.sh downloads and verifies exactly the names in FILES.
	files := regexp.MustCompile(`(?m)^\s*FILES="([^"]*)"`).FindSubmatch(script)
	if files == nil {
		t.Fatal("install.sh no longer has a FILES= list; nothing says what it downloads")
	}

	// The per-board binary is named three different ways on purpose — a
	// glob in the Makefile, a shell variable in install.sh — and which
	// names it may take is already pinned by
	// cmd/spoloh/arch_test.go. Drop it here so this test is about the four
	// deployment files only.
	binary := func(name string) bool {
		return strings.HasPrefix(name, "spoloh-linux-") || name == "spoloh-linux-*" || name == "spoloh-$ARCH"
	}
	names := func(field []byte, trimPrefix string) []string {
		var out []string
		for _, f := range strings.Fields(string(field)) {
			f = strings.TrimPrefix(f, trimPrefix)
			if !binary(f) {
				out = append(out, f)
			}
		}
		sort.Strings(out)
		return out
	}

	copied := names(cp[1], "deploy/")
	hashed := names(sum[1], "")
	fetched := names(files[1], "")

	// install.sh is the one file that is in the release but not in FILES:
	// it is the script doing the downloading, not something it downloads.
	// Checked rather than silently skipped, so it cannot fall out of the
	// release unnoticed — the README points people at
	// releases/latest/download/install.sh, and a release without it is a
	// 404 for every new install.
	const self = "install.sh"
	if !slices.Contains(copied, self) || !slices.Contains(hashed, self) {
		t.Errorf("the release no longer ships %s (copied %v, hashed %v); the README fetches it from releases/latest/download", self, copied, hashed)
	}
	if slices.Contains(fetched, self) {
		t.Errorf("install.sh downloads itself; FILES = %v", fetched)
	}
	copied = slices.DeleteFunc(copied, func(n string) bool { return n == self })
	hashed = slices.DeleteFunc(hashed, func(n string) bool { return n == self })

	if !slices.Equal(copied, hashed) {
		t.Errorf("the Makefile copies %v into dist/ but publishes checksums for %v; a file in one list and not the other either ships unverifiable or is a checksum for something that is not there", copied, hashed)
	}
	if !slices.Equal(copied, fetched) {
		t.Errorf("the Makefile ships %v but install.sh downloads %v; every install in the field would 404 on the difference", copied, fetched)
	}
}

// The polkit rule is the most security-sensitive file in this directory.
// org.freedesktop.systemd1.manage-units, granted unconditionally, would let
// anything that reaches the dashboard over HTTP start, stop or restart any
// unit on the board — sshd, the firewall, this daemon's own supervision —
// from a device whose whole job is to be reachable by whoever is standing in
// front of it. The grant is therefore pinned to one unit name and one verb,
// and this test is what keeps it pinned: an edit that widens it passes
// gofmt, go vet and every other test in this repository, and would be
// discovered only by somebody reading the file.
//
// Nothing here can prove the rule works — polkit cannot be exercised from a
// development machine, and neither this rule nor the NetworkManager one
// beside it has been confirmed on hardware. What it proves is the narrower
// and more checkable claim: that the file grants exactly the four actions it
// is supposed to, and that the dangerous one is qualified.
func TestTheTimedatePolkitRuleGrantsOnlyItsFourActions(t *testing.T) {
	rule, err := os.ReadFile("50-spoloh-timedate.rules")
	if err != nil {
		t.Fatalf("read the rule: %v", err)
	}

	// Comments name every action in prose; only the code may be read for
	// what is actually granted.
	var code strings.Builder
	for _, line := range strings.Split(string(rule), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		code.WriteString(line)
		code.WriteString("\n")
	}
	body := code.String()

	granted := regexp.MustCompile(`action\.id === "([^"]+)"`).FindAllStringSubmatch(body, -1)
	var ids []string
	for _, m := range granted {
		ids = append(ids, m[1])
	}
	sort.Strings(ids)
	want := []string{
		"org.freedesktop.systemd1.manage-units",
		"org.freedesktop.timedate1.set-ntp",
		"org.freedesktop.timedate1.set-time",
		"org.freedesktop.timedate1.set-timezone",
	}
	if !slices.Equal(ids, want) {
		t.Errorf("the rule names %v, want exactly %v; every extra action is one more thing an HTTP request can do as root", ids, want)
	}

	// The manage-units grant has to be qualified before it can return YES.
	// Read from the action id to the decision that follows it: everything
	// the grant depends on is in between.
	idx := strings.Index(body, `"org.freedesktop.systemd1.manage-units"`)
	if idx < 0 {
		t.Fatal("the rule no longer grants manage-units; the NTP server cannot be applied without it")
	}
	rest := body[idx:]
	end := strings.Index(rest, "polkit.Result.YES")
	if end < 0 {
		t.Fatal("manage-units is named but nothing grants it; the NTP server cannot be applied")
	}
	condition := rest[:end]
	for _, qualifier := range []string{
		`action.lookup("unit") === "spoloh-timesync.service"`,
		`action.lookup("verb") === "start"`,
	} {
		if !strings.Contains(condition, qualifier) {
			t.Errorf("manage-units is granted without %s; that grant lets an HTTP request act on any unit on the board", qualifier)
		}
	}

	// An unlisted action must fall through to the system's own decision
	// rather than to a grant.
	if !strings.Contains(body, "polkit.Result.NOT_HANDLED") {
		t.Error("the rule has no NOT_HANDLED default; an action it does not name must not be decided here")
	}
	if !strings.Contains(body, `subject.user !== "spoloh"`) {
		t.Error("the rule does not check the subject is the spoloh service account")
	}
}

// The unit name is written down in three places in two languages — the
// polkit rule that pins the grant to it, the Go constant the daemon starts,
// and the unit file itself. Two of them agreeing and the third not is a
// daemon whose every NTP-server save is denied by polkit, with nothing in
// the Go build or this shell script to say why.
func TestThePolkitRuleAndTheDaemonAgreeOnTheTimesyncUnitName(t *testing.T) {
	const unit = "spoloh-timesync.service"

	if _, err := os.Stat(unit); err != nil {
		t.Fatalf("the unit file itself is missing: %v", err)
	}
	rule, err := os.ReadFile("50-spoloh-timedate.rules")
	if err != nil {
		t.Fatalf("read the rule: %v", err)
	}
	if !bytes.Contains(rule, []byte(`action.lookup("unit") === "`+unit+`"`)) {
		t.Errorf("the polkit rule does not pin its manage-units grant to %s", unit)
	}
	source, err := os.ReadFile("../internal/timedate/timedatectl.go")
	if err != nil {
		t.Fatalf("read the daemon's side: %v", err)
	}
	if !bytes.Contains(source, []byte(`timesyncUnit = "`+unit+`"`)) {
		t.Errorf("internal/timedate no longer starts %s; polkit grants that name and no other", unit)
	}
	script, err := os.ReadFile("install.sh")
	if err != nil {
		t.Fatalf("read install.sh: %v", err)
	}
	if !bytes.Contains(script, []byte("/etc/systemd/system/"+unit)) {
		t.Errorf("install.sh does not place %s where systemd will find it", unit)
	}
}

// The oneshot runs as root and writes a file root parses. What keeps that
// safe is that the shape of the file is decided here, by root, and the
// unprivileged daemon contributes only the value on the right-hand side of
// NTP=. A unit that took the whole stanza from /etc/spoloh would hand that
// decision to the side of the boundary that must not have it.
func TestTheTimesyncUnitWritesTheStanzaItselfAndClearsItWhenEmpty(t *testing.T) {
	unit, err := os.ReadFile("spoloh-timesync.service")
	if err != nil {
		t.Fatalf("read the unit: %v", err)
	}
	body := string(unit)
	var exec []string
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "ExecStart=") {
			exec = append(exec, line)
		}
	}
	if len(exec) == 0 {
		t.Fatal("the unit runs nothing")
	}
	joined := strings.Join(exec, "\n")

	for _, want := range []string{
		`echo "[Time]"`,                       // the stanza is written here, by root
		`echo "NTP=$s"`,                       // the daemon supplies only this value
		"/etc/spoloh/ntp-server",              // ... and only through this file
		"/etc/systemd/timesyncd.conf.d",       // the directory the daemon cannot write
		"rm -f $f",                            // an empty server removes the drop-in
		"systemctl restart systemd-timesyncd", // timesyncd re-reads only at start
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("the unit's ExecStart does not carry %q:\n%s", want, joined)
		}
	}
	// A % in a unit file is a systemd specifier, not a printf verb, and
	// backslash escapes are resolved before the shell ever sees the line.
	// Either one silently changes what root runs.
	if strings.Contains(joined, "%") {
		t.Errorf("the unit's ExecStart contains a %% specifier, which systemd expands before sh sees it:\n%s", joined)
	}
	if strings.Contains(joined, `\`) {
		t.Errorf("the unit's ExecStart contains a backslash escape, which systemd resolves before sh sees it:\n%s", joined)
	}
	// A section header, not the word in a comment: the unit's own comments
	// explain why it has no [Install] section.
	for _, line := range strings.Split(body, "\n") {
		if strings.TrimSpace(line) == "[Install]" {
			t.Error("the oneshot has an [Install] section; nothing should start it at boot but the daemon itself")
		}
	}
}

// The control socket crosses a seam nothing else checks, in three languages.
// internal/control puts the socket at a compiled-in path; the unit file is
// what makes that path exist, with the ownership that is the CLI's entire
// authorisation model; and an operator adds themselves to a group named in
// neither place unless it is written down.
//
// Each half of it fails silently. A RuntimeDirectory that stopped matching
// control.DefaultSocket leaves the daemon unable to create its socket on
// every boot — a retry loop logging forever and a CLI that never works. A
// Group= changed back to audio makes "may run the CLI" mean "may open every
// sound card on the board". And SupplementaryGroups=audio going missing is
// the one that matters most: the daemon loses its ALSA access and the device
// goes quiet, which is the worst outcome this project has.
func TestTheUnitPublishesTheControlSocketWithTheGroupThatAuthorisesIt(t *testing.T) {
	unit, err := os.ReadFile("spoloh.service")
	if err != nil {
		t.Fatalf("read the unit: %v", err)
	}
	// Directives only. Every one of these words also appears in the prose
	// above it, which proves nothing about what systemd reads.
	var body strings.Builder
	for _, line := range strings.Split(string(unit), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		body.WriteString(line)
		body.WriteString("\n")
	}
	directive := func(name string) string {
		m := regexp.MustCompile(`(?m)^` + name + `=(.*)$`).FindStringSubmatch(body.String())
		if m == nil {
			return ""
		}
		return strings.TrimSpace(m[1])
	}

	// internal/control's DefaultSocket is /run/<RuntimeDirectory>/... — the
	// only thing that makes the daemon's path and systemd's directory agree.
	const socket = "/run/spoloh/control.sock"
	runtimeDir := directive("RuntimeDirectory")
	if runtimeDir == "" {
		t.Fatalf("the unit declares no RuntimeDirectory; nothing creates the directory %s lives in, so the daemon can never publish a control socket", socket)
	}
	if want := filepath.Dir(socket); "/run/"+runtimeDir != want {
		t.Errorf("RuntimeDirectory=%s makes /run/%s, but the daemon publishes its socket in %s", runtimeDir, runtimeDir, want)
	}
	if got := directive("RuntimeDirectoryMode"); got != "0750" {
		t.Errorf("RuntimeDirectoryMode=%q, want 0750; the socket's own 0660 is no gate if anyone can walk into the directory holding it", got)
	}
	if got := directive("Group"); got != "spoloh" {
		t.Errorf("Group=%q, want spoloh; the socket takes the unit's group, and that group is what "+
			"an operator is added to in order to use the CLI — it must not be a group that already means something else", got)
	}
	if got := directive("SupplementaryGroups"); !slices.Contains(strings.Fields(got), "audio") {
		t.Errorf("SupplementaryGroups=%q does not include audio; the daemon would lose aplay and amixer and the device would be silent", got)
	}
}

// The access point is its own polkit action, and granting the other three
// does not grant it. Found on a real board: the supervisor waited out its
// grace period, asked at exactly the right moment, and NetworkManager
// answered "Not authorized to share connections via wifi." A device carried
// somewhere new would have been unreachable, with the only screen that could
// fix it behind the access point that could not rise.
//
// Nothing on a development machine exercises polkit, so this reads the rule
// as text — the same shape TestTheTimedatePolkitRuleGrantsOnlyItsFourActions
// uses, and the only check available short of an install.
func TestTheNetworkPolkitRuleGrantsExactlyWhatTheDaemonUses(t *testing.T) {
	rule, err := os.ReadFile("50-spoloh-networkmanager.rules")
	if err != nil {
		t.Fatalf("read the rule: %v", err)
	}

	// Comments name actions in prose — including one this must never grant —
	// so only the code may be read for what is actually allowed.
	var code strings.Builder
	for _, line := range strings.Split(string(rule), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		code.WriteString(line)
		code.WriteString("\n")
	}
	body := code.String()

	var ids []string
	for _, m := range regexp.MustCompile(`action\.id === "([^"]+)"`).FindAllStringSubmatch(body, -1) {
		ids = append(ids, m[1])
	}
	sort.Strings(ids)

	want := []string{
		"org.freedesktop.NetworkManager.network-control",
		"org.freedesktop.NetworkManager.settings.modify.system",
		"org.freedesktop.NetworkManager.wifi.scan",
		"org.freedesktop.NetworkManager.wifi.share.protected",
	}
	if !slices.Equal(ids, want) {
		t.Errorf("the rule grants %v, want exactly %v", ids, want)
	}

	// wifi.share.open would permit an access point with no password at all.
	// NMCLI.StartHotspot refuses an empty PSK outright, so it is an action
	// this daemon can never need — and a grant it cannot use is one that can
	// only ever be misused.
	if strings.Contains(body, "wifi.share.open") {
		t.Error("the rule grants wifi.share.open, which would allow an access point with no password")
	}
}
