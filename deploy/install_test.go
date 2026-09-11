package deploy

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The installer is shell, runs as root and talks to apt, systemd and the
// network — none of which a test may do. So it runs here against a directory
// of fakes on PATH and a local download directory, which is enough to prove
// the parts that actually go wrong: the architecture mapping, the checksum
// gate, which files land where, and that the access point password is read
// back and printed.
func runInstaller(t *testing.T, downloads, uname string, extraEnv ...string) (string, error) {
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
		"hostname":         "#!/bin/sh\nif [ \"$1\" = -I ]; then echo 192.168.1.50; else echo raspberrypi; fi\n",
		"curl":             "#!/bin/sh\nwhile [ $# -gt 0 ]; do case $1 in -o) out=$2; shift 2;; -*) shift;; *) url=$1; shift;; esac; done\ncp \"$SPOLOH_TEST_DOWNLOADS/$(basename \"$url\")\" \"$out\"\n",
		// install -D and sha256sum are GNU coreutils; a developer's macOS has
		// neither. These shims are faithful to the two behaviours the script
		// depends on, so the checksum gate is still genuinely exercised rather
		// than stubbed out.
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

	logPath := filepath.Join(root, "log")
	prefix := filepath.Join(root, "prefix")
	cmd := exec.Command("sh", "install.sh")
	cmd.Env = append(os.Environ(),
		"PATH="+fakeBin+":"+os.Getenv("PATH"),
		"SPOLOH_PREFIX="+prefix,
		"SPOLOH_BASE_URL=https://example.invalid/releases",
		"SPOLOH_TEST_LOG="+logPath,
		"SPOLOH_TEST_DOWNLOADS="+downloads,
		// No config.json will ever appear in these runs, and a test may not
		// sleep on a real clock. The password test overrides this.
		"SPOLOH_AP_WAIT=0",
	)
	cmd.Env = append(cmd.Env, extraEnv...)
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
		"systemctl enable --now spoloh",
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
