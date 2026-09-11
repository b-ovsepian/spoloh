package timedate_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/b-ovsepian/spoloh/internal/timedate"
)

// recorder is the RunFunc seam every test here drives: it records the
// invocation and answers with scripted output. No test in this package runs
// timedatectl or systemctl.
type recorder struct {
	calls  [][]string
	out    []byte
	err    error
	outFor map[string][]byte
}

func (r *recorder) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	r.calls = append(r.calls, append([]string{name}, args...))
	if r.outFor != nil {
		if out, ok := r.outFor[strings.Join(args, " ")]; ok {
			return out, r.err
		}
	}
	return r.out, r.err
}

func (r *recorder) last() string {
	if len(r.calls) == 0 {
		return ""
	}
	return strings.Join(r.calls[len(r.calls)-1], " ")
}

// The capture in testdata is verbatim `timedatectl show` output from the
// operator's own Raspberry Pi 4 on Debian 13. Parsing it is the whole reason
// this package reads `show` rather than `status`, whose output is localised
// prose, so the parser is pinned against the real bytes rather than against
// a line somebody typed into a test.
func TestShowParsesTheRealBoardsOutput(t *testing.T) {
	out, err := os.ReadFile(filepath.Join("testdata", "timedatectl-show-rpi4.txt"))
	if err != nil {
		t.Fatalf("read capture: %v", err)
	}
	rec := &recorder{out: out}
	td := timedate.Timedatectl{Run: rec.run}

	st, err := td.Show(context.Background())
	if err != nil {
		t.Fatalf("Show: %v", err)
	}
	if st.Timezone != "Europe/Kyiv" {
		t.Errorf("Timezone = %q, want Europe/Kyiv", st.Timezone)
	}
	if st.LocalRTC {
		t.Error("LocalRTC = true, the capture says no")
	}
	if !st.CanNTP {
		t.Error("CanNTP = false, the capture says yes")
	}
	if !st.NTP {
		t.Error("NTP = false, the capture says yes")
	}
	if !st.NTPSynchronized {
		t.Error("NTPSynchronized = false, the capture says yes")
	}
	if got := rec.last(); got != "timedatectl show" {
		t.Errorf("ran %q, want `timedatectl show` — status is localised prose and must not be parsed", got)
	}
}

// A board that has not synchronised, has NTP switched off, and keeps local
// time in its RTC: every boolean the other way round from the capture above,
// so a parser that hard-coded the common answer cannot pass both.
func TestShowReadsTheNegativeAnswers(t *testing.T) {
	rec := &recorder{out: []byte("Timezone=UTC\nLocalRTC=yes\nCanNTP=no\nNTP=no\nNTPSynchronized=no\n")}
	td := timedate.Timedatectl{Run: rec.run}

	st, err := td.Show(context.Background())
	if err != nil {
		t.Fatalf("Show: %v", err)
	}
	if st.Timezone != "UTC" || !st.LocalRTC || st.CanNTP || st.NTP || st.NTPSynchronized {
		t.Errorf("Show = %+v, want UTC with every flag off but LocalRTC", st)
	}
}

// systemd grows properties between versions; a parser that rejected a line
// it did not recognise would turn an upgrade into a dashboard that reports
// nothing about the clock at all.
func TestShowIgnoresUnknownAndMalformedLines(t *testing.T) {
	rec := &recorder{out: []byte("SomethingNew=42\nnot a key value line\n\nTimezone=Europe/Kyiv\nNTP=yes\n")}
	td := timedate.Timedatectl{Run: rec.run}

	st, err := td.Show(context.Background())
	if err != nil {
		t.Fatalf("Show: %v", err)
	}
	if st.Timezone != "Europe/Kyiv" || !st.NTP {
		t.Errorf("Show = %+v, want the known keys read past the unknown ones", st)
	}
}

func TestShowReportsTheCommandsOwnMessage(t *testing.T) {
	rec := &recorder{out: []byte("Failed to query server: Access denied"), err: errors.New("exit status 1")}
	td := timedate.Timedatectl{Run: rec.run}

	_, err := td.Show(context.Background())
	if err == nil {
		t.Fatal("Show accepted a failing timedatectl")
	}
	if !strings.Contains(err.Error(), "Access denied") {
		t.Errorf("error %q drops timedatectl's own message, which is the only thing naming a polkit denial", err)
	}
}

func TestSettersRunTheExpectedCommands(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func(timedate.Timedatectl) error
		want string
	}{
		{"timezone", func(td timedate.Timedatectl) error {
			return td.SetTimezone(context.Background(), "Europe/Kyiv")
		}, "timedatectl set-timezone Europe/Kyiv"},
		{"ntp on", func(td timedate.Timedatectl) error {
			return td.SetNTP(context.Background(), true)
		}, "timedatectl set-ntp true"},
		{"ntp off", func(td timedate.Timedatectl) error {
			return td.SetNTP(context.Background(), false)
		}, "timedatectl set-ntp false"},
		{"time", func(td timedate.Timedatectl) error {
			return td.SetTime(context.Background(), "2026-09-12 00:14:22")
		}, "timedatectl set-time 2026-09-12 00:14:22"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &recorder{}
			if err := tc.call(timedate.Timedatectl{Run: rec.run}); err != nil {
				t.Fatalf("call: %v", err)
			}
			if got := rec.last(); got != tc.want {
				t.Errorf("ran %q, want %q", got, tc.want)
			}
		})
	}
}

func TestZonesReadsTheHostsList(t *testing.T) {
	rec := &recorder{out: []byte("Africa/Abidjan\nEurope/Kyiv\nUTC\n")}
	zones, err := timedate.Timedatectl{Run: rec.run}.Zones(context.Background())
	if err != nil {
		t.Fatalf("Zones: %v", err)
	}
	if len(zones) != 3 || zones[1] != "Europe/Kyiv" {
		t.Errorf("Zones = %v, want the three lines of the list", zones)
	}
	// Without --no-pager timedatectl pipes this list through a pager, and a
	// daemon reading a pager's output gets control sequences or a hang.
	if got := rec.last(); !strings.Contains(got, "--no-pager") {
		t.Errorf("ran %q, want list-timezones with --no-pager", got)
	}
}

// ApplyNTPServer is the privilege-crossing half of this package: the daemon
// writes one validated token, and a root oneshot turns it into a file in
// /etc/systemd/timesyncd.conf.d/. Both halves have to happen, in that order
// — a unit started before the file is written would adopt the previous
// server.
func TestApplyNTPServerWritesTheServerThenStartsTheUnit(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "ntp-server")
	rec := &recorder{}
	td := timedate.Timedatectl{Run: rec.run, ServerFile: file}

	if err := td.ApplyNTPServer(context.Background(), "ntp.example.org"); err != nil {
		t.Fatalf("ApplyNTPServer: %v", err)
	}
	body, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read server file: %v", err)
	}
	if strings.TrimSpace(string(body)) != "ntp.example.org" {
		t.Errorf("server file holds %q, want the bare server", body)
	}
	if got := rec.last(); got != "systemctl start spoloh-timesync.service" {
		t.Errorf("ran %q, want the oneshot unit by its exact name — the polkit rule is pinned to it", got)
	}
}

// An empty server is how an operator says "use whatever the distribution
// configures". The file is emptied rather than left holding the old value,
// which is the unit's signal to remove the drop-in.
func TestApplyNTPServerClearsTheFileForAnEmptyServer(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "ntp-server")
	if err := os.WriteFile(file, []byte("old.example.org\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := &recorder{}
	td := timedate.Timedatectl{Run: rec.run, ServerFile: file}

	if err := td.ApplyNTPServer(context.Background(), ""); err != nil {
		t.Fatalf("ApplyNTPServer: %v", err)
	}
	body, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read server file: %v", err)
	}
	if len(body) != 0 {
		t.Errorf("server file still holds %q after being cleared", body)
	}
}

// The file this writes is parsed by root. A value carrying a newline would
// append a directive of the submitter's choosing to a root-owned config
// file, so the boundary that does the writing checks for itself rather than
// trusting that config.Validate already has — and nothing may reach the
// disk or the unit when it refuses.
func TestApplyNTPServerRefusesAnInjectedValueAndWritesNothing(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "ntp-server")
	rec := &recorder{}
	td := timedate.Timedatectl{Run: rec.run, ServerFile: file}

	for _, bad := range []string{
		"ntp.example.org\nNTP=evil.example.org",
		"ntp.example.org\r\nFallbackNTP=evil",
		"ntp.example.org evil.example.org",
		`ntp.example.org"`,
		"NTP=evil.example.org",
		"ntp.example.org;reboot",
	} {
		if err := td.ApplyNTPServer(context.Background(), bad); err == nil {
			t.Errorf("ApplyNTPServer accepted %q", bad)
		}
		if _, err := os.Stat(file); !os.IsNotExist(err) {
			t.Fatalf("ApplyNTPServer(%q) wrote the server file anyway", bad)
		}
		if len(rec.calls) != 0 {
			t.Fatalf("ApplyNTPServer(%q) started the unit anyway: %v", bad, rec.calls)
		}
	}
}

// The Fake stands in for systemd everywhere above this package, so it has to
// refuse what systemd refuses: set-time while NTP is on.
func TestFakeRefusesManualTimeWhileNTPIsOn(t *testing.T) {
	f := &timedate.Fake{CurrentState: timedate.State{NTP: true}}
	if err := f.SetTime(context.Background(), "2026-09-12 00:14:22"); !errors.Is(err, timedate.ErrNTPEnabled) {
		t.Fatalf("SetTime with NTP on = %v, want ErrNTPEnabled", err)
	}
	if err := f.SetNTP(context.Background(), false); err != nil {
		t.Fatalf("SetNTP: %v", err)
	}
	if err := f.SetTime(context.Background(), "2026-09-12 00:14:22"); err != nil {
		t.Fatalf("SetTime with NTP off = %v, want it accepted", err)
	}
}
