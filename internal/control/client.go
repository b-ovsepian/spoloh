package control

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net"
	"strings"
	"time"
)

// dialTimeout and exchangeDeadline bound the client's side of one exchange.
// The daemon is on the same board and answers from memory, so anything that
// takes seconds is a daemon in trouble and the operator should be told rather
// than left with a hung terminal.
const (
	dialTimeout       = 2 * time.Second
	exchangeDeadline  = 10 * time.Second
	defaultLogEntries = 20
)

// Run executes one CLI command and returns the process exit code. Everything
// a person should see is written to out, which is how the test reads it.
//
// Output is line-oriented and free of colour and box drawing on purpose: it
// is read at a terminal and by shell scripts, and "key: value" is the one
// shape both can handle. Every key a command prints is always printed, with
// "-" standing in for empty, so a script's field parse does not change shape
// with the device's state.
func Run(command string, args []string, out io.Writer) int {
	flags := flag.NewFlagSet("spoloh "+command, flag.ContinueOnError)
	flags.SetOutput(out)
	socket := flags.String("socket", DefaultSocket, "path of the daemon's control socket")
	n := flags.Int("n", defaultLogEntries, "how many log entries to show")
	if err := flags.Parse(args); err != nil {
		return 2
	}

	req := Request{Command: command}
	switch command {
	case CmdPlay:
		if flags.NArg() != 1 {
			fmt.Fprintln(out, "usage: spoloh play <sound>   (spoloh sounds lists them)")
			return 2
		}
		req.Sound = flags.Arg(0)
	case CmdLog:
		req.N = *n
	}

	resp, err := Do(*socket, req)
	if err != nil {
		fmt.Fprintln(out, unreachable(*socket, err))
		return 1
	}
	if resp.Error != "" {
		fmt.Fprintln(out, resp.Error)
		return 1
	}
	render(out, command, req, resp)
	return 0
}

// Do sends one request and reads one response.
func Do(socket string, req Request) (Response, error) {
	conn, err := net.DialTimeout("unix", socket, dialTimeout)
	if err != nil {
		return Response{}, err
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(exchangeDeadline)); err != nil {
		return Response{}, err
	}
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return Response{}, err
	}
	var resp Response
	if err := json.NewDecoder(io.LimitReader(conn, maxRequest)).Decode(&resp); err != nil {
		return Response{}, err
	}
	return resp, nil
}

// unreachable turns a dial failure into the sentence that actually fixes it.
//
// The two ways this fails have opposite remedies and an identical-looking
// error, so they are separated here rather than left for the operator to
// work out. A missing socket is a daemon that is not running (or one that
// could not create it, which the journal will say). A permission denial is
// the authorisation model doing its job: the account is not in the group the
// socket belongs to, and adding it needs a new login to take effect, which is
// the part people lose an afternoon to.
func unreachable(socket string, err error) string {
	var b strings.Builder
	fmt.Fprintf(&b, "cannot reach the daemon on %s: %v\n", socket, err)
	switch {
	case errors.Is(err, fs.ErrPermission):
		b.WriteString("your account is not allowed to use this socket.\n")
		b.WriteString("try: sudo usermod -aG spoloh $USER   then log out and back in")
	case errors.Is(err, fs.ErrNotExist):
		b.WriteString("the daemon is not running, or it could not create its socket.\n")
		b.WriteString("try: systemctl status spoloh")
	default:
		b.WriteString("the daemon may have stopped without cleaning up.\n")
		b.WriteString("try: systemctl status spoloh")
	}
	return b.String()
}

func render(out io.Writer, command string, req Request, resp Response) {
	switch command {
	case CmdStatus:
		printStatus(out, resp.Status)
	case CmdSounds:
		for _, s := range resp.Sounds {
			fmt.Fprintln(out, s)
		}
	case CmdPlay:
		fmt.Fprintf(out, "playing %s\n", req.Sound)
	case CmdStop:
		fmt.Fprintln(out, "stopped")
	case CmdLog:
		printLog(out, resp.Entries)
	case CmdVersion:
		if resp.Version != nil {
			fmt.Fprintf(out, "version: %s\narch: %s\n", dash(resp.Version.Version), dash(resp.Version.Arch))
		}
	}
}

func printStatus(out io.Writer, s *Status) {
	if s == nil {
		return
	}
	fmt.Fprintf(out, "alert: %s\n", yesno(s.Alert))
	fmt.Fprintf(out, "since: %s\n", stamp(s.Since))
	fmt.Fprintf(out, "region: %s\n", dash(s.Region))
	fmt.Fprintf(out, "source: %s\n", dash(s.Source))
	fmt.Fprintf(out, "last_update: %s\n", stamp(s.LastUpdate))
	fmt.Fprintf(out, "source_error: %s\n", dash(s.SourceError))
	fmt.Fprintf(out, "clock_synced: %s\n", yesno(s.ClockSynced))
	fmt.Fprintf(out, "output: %s\n", dash(s.Output))
	fmt.Fprintf(out, "playing: %s\n", dash(s.Playing))
}

// printLog writes one tab-separated row per entry, newest first, in the order
// the daemon returned them. Tabs rather than aligned columns: the width of a
// sound name is not something a script should have to parse around, and cut
// and awk both split on a tab by default.
func printLog(out io.Writer, entries []Entry) {
	for _, e := range entries {
		fmt.Fprintf(out, "%s\t%s\t%s\t%s\t%s\t%s\n",
			stamp(e.At), dash(e.Kind), okfail(e.OK), dash(e.Event), dash(e.Sound), dash(e.Err))
	}
}

func yesno(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func okfail(b bool) string {
	if b {
		return "ok"
	}
	return "failed"
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// stamp prints a time in whatever location the daemon sent it in — the site's
// own, which is what an operator reading a log at that site needs. A zero
// time is not "1 January year 1", it is "this has not happened yet".
func stamp(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Format(time.RFC3339)
}

// Usage is the one-line-per-command summary main prints for an unknown verb.
var Usage = strings.Join([]string{
	"  spoloh status          what the device is doing right now",
	"  spoloh sounds          the sounds in the library, one per line",
	"  spoloh play <sound>    play one of them through the real output",
	"  spoloh stop            silence whatever is playing",
	"  spoloh log [-n N]      the last N event log entries",
	"  spoloh version         the running daemon's version and architecture",
	"  spoloh update          install a newer release",
}, "\n")
