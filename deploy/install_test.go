package deploy

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
		"hostnamectl":      "#!/bin/sh\necho \"hostnamectl $*\" >> \"$SPOLOH_TEST_LOG\"\n",
		// The non-`-I` branch defaults to a hostname that is not "spoloh",
		// so the every-test-but-one path exercises the rename; overridden via
		// SPOLOH_TEST_HOSTNAME by the idempotence test below.
		"hostname": "#!/bin/sh\nif [ \"$1\" = -I ]; then echo 192.168.1.50; else echo \"${SPOLOH_TEST_HOSTNAME:-raspberrypi}\"; fi\n",
		"curl":     "#!/bin/sh\nwhile [ $# -gt 0 ]; do case $1 in -o) out=$2; shift 2;; -*) shift;; *) url=$1; shift;; esac; done\ncp \"$SPOLOH_TEST_DOWNLOADS/$(basename \"$url\")\" \"$out\"\n",
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
