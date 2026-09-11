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
		"hostname": "#!/bin/sh\nif [ \"$1\" = -I ]; then echo 192.168.1.50; elif [ -f \"$SPOLOH_TEST_HOSTNAME_FILE\" ]; then cat \"$SPOLOH_TEST_HOSTNAME_FILE\"; else echo \"${SPOLOH_TEST_HOSTNAME:-raspberrypi}\"; fi\n",
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

			report, err := runInstaller(t, downloadDir(t, ""), "armv7l", env...)
			if err != nil {
				t.Fatalf("installer failed: %v\n%s", err, report)
			}

			if got := readFile(hosts); got != tc.want {
				t.Errorf("/etc/hosts is\n%q\nwant\n%q\n%s", got, tc.want, report)
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
// volunteer at a school, which is the one place a bad install cannot be
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
