package main

import (
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The daemon's own flags must keep working exactly as they did: an operator
// typing `spoloh -config ...` in a unit file that predates this subcommand
// must not suddenly be running an update.
func TestSubcommandIsOnlyTakenFromABareFirstArgument(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"spoloh"}, ""},
		{[]string{"spoloh", "-config", "/etc/spoloh/config.json"}, ""},
		{[]string{"spoloh", "-version"}, ""},
		{[]string{"spoloh", "update"}, "update"},
		{[]string{"spoloh", "update", "--check"}, "update"},
	} {
		if got := subcommand(tc.args); got != tc.want {
			t.Errorf("subcommand(%q) = %q, want %q", tc.args, got, tc.want)
		}
	}
}

func TestUpdateCheckChangesNothingWhenItCannotReachTheReleases(t *testing.T) {
	// --check is what a dashboard button will call before offering to update,
	// so it must never touch the binary, least of all when the network is the
	// thing that failed.
	//
	// The base URL is loopback on a port nothing listens on, so this resolves
	// no name and reaches no network: it fails with connection refused in
	// microseconds. A hostname here would send a DNS query out of the machine.
	dir := t.TempDir()
	bin := dir + "/spoloh"
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho v1.0.0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	var out strings.Builder
	code := runUpdate([]string{"--check", "--bin", bin, "--base-url", "http://127.0.0.1:1/releases"}, &out, &http.Client{})

	if code == 0 {
		t.Errorf("--check reported success with no reachable release server; output: %s", out.String())
	}
	if _, err := os.Stat(bin + ".previous"); err == nil {
		t.Error("--check made a rollback copy; it must change nothing")
	}
	if !strings.Contains(out.String(), "could not check for updates") {
		t.Errorf("output does not say what went wrong: %s", out.String())
	}
}

// stubTransport serves a fixed map of URLs so this file's HTTP-touching
// tests never open a socket. Same pattern internal/updater's own tests use
// (internal/updater/updater_test.go); redefined here because it is not
// exported and the two packages share nothing else.
type stubTransport struct {
	pages map[string]stubPage
}

type stubPage struct {
	status   int
	body     string
	location string
}

func (s *stubTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	p, ok := s.pages[r.URL.String()]
	if !ok {
		return &http.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader("not found")), Header: http.Header{}, Request: r}, nil
	}
	h := http.Header{}
	if p.location != "" {
		h.Set("Location", p.location)
	}
	return &http.Response{StatusCode: p.status, Body: io.NopCloser(strings.NewReader(p.body)), Header: h, Request: r}, nil
}

func TestUpdateCheckLeavesTheFilesystemUntouchedOnSuccess(t *testing.T) {
	// TestUpdateCheckChangesNothingWhenItCannotReachTheReleases only proves
	// --check is safe when Latest fails outright — it can never observe
	// whether the "if *check { return 0 }" guard exists, because Latest
	// returns before that guard is ever reached. This test exercises the
	// path where Latest succeeds, which is the only path the guard actually
	// protects: an operator's dashboard hitting a real, reachable release
	// server with --check set.
	const base = "http://update-check.invalid/releases"
	dir := t.TempDir()
	bin := dir + "/spoloh"
	const installed = "#!/bin/sh\necho v1.0.0\n"
	if err := os.WriteFile(bin, []byte(installed), 0o755); err != nil {
		t.Fatal(err)
	}

	// runUpdate reads the package-level arch and version to build the
	// Updater, the same way the real binary does with its build-time stamp.
	// A test binary has arch == "dev", which Latest refuses before ever
	// making a request, so this test has to stand in for a published build.
	oldArch, oldVersion := arch, version
	arch, version = "linux-armv7", "v1.0.0"
	t.Cleanup(func() { arch, version = oldArch, oldVersion })

	client := &http.Client{Transport: &stubTransport{pages: map[string]stubPage{
		base + "/latest":     {status: 302, location: base + "/tag/v9.9.9"},
		base + "/tag/v9.9.9": {status: 200, body: "release page"},
	}}}

	var out strings.Builder
	code := runUpdate([]string{"--check", "--bin", bin, "--base-url", base}, &out, client)

	if code != 0 {
		t.Fatalf("--check with a reachable release server returned %d, want 0; output: %s", code, out.String())
	}
	if !strings.Contains(out.String(), "available: v9.9.9") {
		t.Errorf("output does not report the available version: %s", out.String())
	}
	got, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != installed {
		t.Errorf("--check changed the installed binary: %q", got)
	}
	if entries, _ := filepath.Glob(bin + "*"); len(entries) != 1 {
		t.Errorf("--check left files behind next to the binary: %v", entries)
	}
}

func TestUpdateRejectsCheckAndRollbackTogether(t *testing.T) {
	// The two flags say contradictory things: --check promises to change
	// nothing, --rollback restores the previous binary and restarts the
	// unit. A caller that forwards a dry-run flag alongside whatever else it
	// was asked to do — an ordinary thing for a wrapper script or a
	// dashboard to do — must not get a real rollback out of what looked like
	// a check.
	dir := t.TempDir()
	bin := dir + "/spoloh"
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho v1.0.0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	var out strings.Builder
	code := runUpdate([]string{"--check", "--rollback", "--bin", bin}, &out, &http.Client{})

	if code == 0 {
		t.Errorf("--check --rollback reported success; output: %s", out.String())
	}
	if _, err := os.Stat(bin + ".previous"); err == nil {
		t.Error("--check --rollback made a rollback copy")
	}
	if !strings.Contains(out.String(), "--check") || !strings.Contains(out.String(), "--rollback") {
		t.Errorf("output does not name the conflicting flags: %s", out.String())
	}
}

// asRoot makes the root check pass for the rest of the test. A test may not
// run as root, and the paths behind that check — the actual update and the
// rollback — are the ones most worth exercising.
func asRoot(t *testing.T) {
	t.Helper()
	old := geteuid
	geteuid = func() int { return 0 }
	t.Cleanup(func() { geteuid = old })
}

func TestUpdateRefusesToRunWithoutRootAndDownloadsNothing(t *testing.T) {
	// A forgotten sudo used to fail only after the whole ~28 MB binary had
	// come down a school's connection, with "permission denied" and neither
	// the word "root" nor the word "sudo" anywhere in it.
	dir := t.TempDir()
	bin := dir + "/spoloh"
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho v1.0.0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Any request at all is a failure here: the point is that the refusal
	// comes before the download, not after it.
	client := &http.Client{Transport: refusingTransport{t}}

	var out strings.Builder
	code := runUpdate([]string{"--bin", bin, "--base-url", "http://example.invalid/releases"}, &out, client)

	if code == 0 {
		t.Errorf("update reported success as an unprivileged user: %s", out.String())
	}
	for _, want := range []string{"root", "sudo"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output never says %q, so nobody learns what to do: %s", want, out.String())
		}
	}
}

// refusingTransport fails the test if the update reaches the network at all.
type refusingTransport struct{ t *testing.T }

func (r refusingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r.t.Errorf("the update downloaded %s before checking for root", req.URL)
	return nil, errors.New("no network in tests")
}

func TestRollbackWithNothingToRollBackToFailsClearly(t *testing.T) {
	// No network at all. An operator who runs this on a device that has never
	// been updated must be told that, not handed a stack trace.
	asRoot(t)
	dir := t.TempDir()
	bin := dir + "/spoloh"
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho v1.0.0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	var out strings.Builder
	if code := runUpdate([]string{"--rollback", "--bin", bin}, &out, &http.Client{}); code == 0 {
		t.Error("--rollback reported success with no previous binary")
	}
	if !strings.Contains(out.String(), "no previous binary") {
		t.Errorf("output does not explain why: %s", out.String())
	}
}
