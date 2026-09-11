package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/b-ovsepian/spoloh/internal/updater"
)

// defaultReleases is where published builds live. Overridable so a fork, or a
// school with a local mirror, needs no rebuild.
const defaultReleases = "https://github.com/b-ovsepian/spoloh/releases"

// subcommand returns the verb in args, or "" when args holds only flags for
// the daemon. A bare first argument is unambiguous: every daemon option
// starts with a dash, so nothing that worked before this existed can now be
// read as a command.
func subcommand(args []string) string {
	if len(args) < 2 {
		return ""
	}
	if len(args[1]) == 0 || args[1][0] == '-' {
		return ""
	}
	return args[1]
}

// runUpdate implements `spoloh update`. It returns the process exit code and
// writes everything a person should see to out, so the test can read it.
func runUpdate(args []string, out io.Writer) int {
	fs := flag.NewFlagSet("spoloh update", flag.ContinueOnError)
	fs.SetOutput(out)
	check := fs.Bool("check", false, "report the installed and available versions, change nothing")
	rollback := fs.Bool("rollback", false, "put the previously installed binary back")
	binPath := fs.String("bin", "/usr/local/bin/spoloh", "path of the binary to replace")
	unit := fs.String("unit", "spoloh", "systemd unit to restart")
	baseURL := fs.String("base-url", defaultReleases, "release download root")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	// Generous, and separate from the per-download timeout: a school's
	// connection is often slow, and an update that gives up early is an
	// update nobody can apply.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	u := updater.Updater{
		BaseURL: *baseURL,
		Arch:    arch,
		Version: version,
		BinPath: *binPath,
		Unit:    *unit,
		HTTP:    &http.Client{Timeout: 5 * time.Minute},
	}

	if *rollback {
		if err := u.Rollback(ctx); err != nil {
			fmt.Fprintf(out, "rollback failed: %v\n", err)
			return 1
		}
		fmt.Fprintln(out, "rolled back to the previous binary")
		return 0
	}

	rel, err := u.Latest(ctx)
	if err != nil {
		fmt.Fprintf(out, "could not check for updates: %v\n", err)
		return 1
	}
	fmt.Fprintf(out, "installed: %s\navailable: %s\n", version, rel.Version)
	if *check {
		return 0
	}
	if rel.Version == version {
		fmt.Fprintln(out, "already up to date")
		return 0
	}
	if err := u.Apply(ctx, rel); err != nil {
		fmt.Fprintf(out, "update failed: %v\n", err)
		return 1
	}
	fmt.Fprintf(out, "updated to %s\n", rel.Version)
	return 0
}
