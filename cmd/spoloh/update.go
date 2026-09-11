package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/b-ovsepian/spoloh/internal/updater"
)

// defaultReleases is where published builds live. Overridable so a fork, or a
// school with a local mirror, needs no rebuild.
const defaultReleases = "https://github.com/b-ovsepian/spoloh/releases"

// geteuid is os.Geteuid, replaced in tests. A test may not require root, and
// the only other way to exercise the paths behind the root check would be to
// skip them on every machine that runs the suite.
var geteuid = os.Geteuid

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
// client is the HTTP client used for every request the update makes; main
// passes the real one, and tests pass one whose Transport is a stub so no
// socket is ever opened.
func runUpdate(args []string, out io.Writer, client *http.Client) int {
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

	// The two flags express contradictory intents, and an operator who typed
	// both meant one of them: honouring --check silently would leave someone
	// who wanted a rollback staring at a version report with nothing
	// restored, and honouring --rollback silently is the dangerous one — a
	// caller that always forwards a dry-run flag alongside whatever else it
	// was asked to do would get a real rollback out of what it thought was a
	// check. Refuse instead of guessing which one was meant.
	if *check && *rollback {
		fmt.Fprintln(out, "--check and --rollback cannot both be set: --check only reports versions and --rollback restores the previous binary; pick one")
		return 2
	}

	// Checked before anything is downloaded. A forgotten sudo used to fail
	// only after the whole ~28 MB binary had come down a school's
	// connection, with "permission denied" and neither the word "root" nor
	// the word "sudo" anywhere in the message. --check is deliberately
	// exempt: it writes nothing, and the dashboard's update button will
	// call it as the unprivileged daemon.
	if !*check && geteuid() != 0 {
		what := "replaces " + *binPath + " and restarts the " + *unit + " unit"
		how := "sudo spoloh update"
		if *rollback {
			what = "restores the previous binary at " + *binPath + " and restarts the " + *unit + " unit"
			how += " --rollback"
		}
		fmt.Fprintf(out, "this must run as root: it %s.\ntry: %s\n", what, how)
		return 1
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
		HTTP:    client,
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
