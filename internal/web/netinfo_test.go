package web_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/b-ovsepian/spoloh/internal/web"
)

func TestLocalAddressesSkipsLoopbackAndReturnsIPv4(t *testing.T) {
	got := web.LocalAddresses()
	for _, addr := range got {
		if strings.HasPrefix(addr, "127.") || addr == "::1" {
			t.Errorf("loopback address %q should not be offered as a way to reach the device", addr)
		}
		if strings.Contains(addr, ":") {
			t.Errorf("address %q is not IPv4; the page shows a number an operator reads over the phone", addr)
		}
	}
}

func TestMDNSNameIsEmptyWithoutAvahi(t *testing.T) {
	// Nothing publishes the name, so printing one would send an operator to
	// an address that does not answer. This line is read aloud over a phone.
	got := web.MDNSNameFrom(filepath.Join(t.TempDir(), "absent"), func() (string, error) { return "spoloh", nil })
	if got != "" {
		t.Errorf("MDNSNameFrom = %q with no avahi socket, want empty", got)
	}
}

func TestMDNSNameUsesTheFirstLabelOfTheHostname(t *testing.T) {
	// avahi publishes <first label>.local: a host called "spoloh.lan" answers
	// to "spoloh.local", never "spoloh.lan.local".
	socket := filepath.Join(t.TempDir(), "socket")
	if err := os.WriteFile(socket, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ host, want string }{
		{"spoloh", "spoloh.local"},
		{"spoloh.lan", "spoloh.local"},
		{"spoloh-2", "spoloh-2.local"},
		{"", ""},
		{".lan", ""},
	} {
		got := web.MDNSNameFrom(socket, func() (string, error) { return tc.host, nil })
		if got != tc.want {
			t.Errorf("hostname %q: MDNSNameFrom = %q, want %q", tc.host, got, tc.want)
		}
	}
}

func TestMDNSNameIsEmptyWhenTheHostnameCannotBeRead(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "socket")
	if err := os.WriteFile(socket, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	got := web.MDNSNameFrom(socket, func() (string, error) { return "", errors.New("nope") })
	if got != "" {
		t.Errorf("MDNSNameFrom = %q when the hostname is unreadable, want empty", got)
	}
}
