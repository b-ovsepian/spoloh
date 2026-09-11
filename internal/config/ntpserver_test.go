package config_test

import (
	"strings"
	"testing"

	"github.com/b-ovsepian/spoloh/internal/config"
)

// The NTP server crosses a privilege boundary: a root oneshot unit reads it
// and turns it into an NTP= line in /etc/systemd/timesyncd.conf.d/. A value
// carrying a newline appends a directive of the submitter's choosing to a
// root-owned config file; a quote or an "=" lets the line be re-read as
// something other than the key it was meant to be. This is the rejection
// the brief asks for at config.Validate, not only at the form, so a
// hand-edited config.json is refused the same way a crafted POST is.
func TestValidateRejectsAnInjectedNTPServer(t *testing.T) {
	for _, bad := range []string{
		"ntp.example.org\nNTP=evil.example.org",
		"ntp.example.org\r\nFallbackNTP=evil.example.org",
		"ntp.example.org evil.example.org",
		`ntp.example.org"`,
		"ntp.example.org'",
		"NTP=evil.example.org",
		"ntp.example.org;reboot",
		"$(reboot)",
		"ntp.example.org\t",
		"ntp.example.org\x00",
		strings.Repeat("a", 254),
		".example.org",
		"-example.org",
		"example.org.",
	} {
		cfg := config.Default()
		cfg.Device.NTPServer = bad
		if err := cfg.Validate(); err == nil {
			t.Errorf("Validate accepted device.ntp_server %q", bad)
			continue
		} else if !strings.Contains(err.Error(), "device.ntp_server") {
			t.Errorf("Validate rejected %q with %q, which does not name the field at fault", bad, err)
		}
	}
}

func TestValidateAcceptsARealNTPServer(t *testing.T) {
	for _, good := range []string{
		"",
		"ntp.example.org",
		"time.cloudflare.com",
		"ua.pool.ntp.org",
		"192.168.1.1",
		"2001:db8::1",
		"ntp-1.example.org",
	} {
		cfg := config.Default()
		cfg.Device.NTPServer = good
		if err := cfg.Validate(); err != nil {
			t.Errorf("Validate rejected device.ntp_server %q: %v", good, err)
		}
	}
}

// Empty is the shipped state and means "whatever the distribution
// configures". A device that came out of the box with no time server set
// must not be refused at startup.
func TestDefaultCarriesNoNTPServer(t *testing.T) {
	if got := config.Default().Device.NTPServer; got != "" {
		t.Errorf("Default().Device.NTPServer = %q, want empty", got)
	}
}
