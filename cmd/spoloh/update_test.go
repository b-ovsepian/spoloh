package main

import (
	"os"
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
	code := runUpdate([]string{"--check", "--bin", bin, "--base-url", "http://127.0.0.1:1/releases"}, &out)

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

func TestRollbackWithNothingToRollBackToFailsClearly(t *testing.T) {
	// No network at all. An operator who runs this on a device that has never
	// been updated must be told that, not handed a stack trace.
	dir := t.TempDir()
	bin := dir + "/spoloh"
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho v1.0.0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	var out strings.Builder
	if code := runUpdate([]string{"--rollback", "--bin", bin}, &out); code == 0 {
		t.Error("--rollback reported success with no previous binary")
	}
	if !strings.Contains(out.String(), "no previous binary") {
		t.Errorf("output does not explain why: %s", out.String())
	}
}
