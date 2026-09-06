package web_test

import (
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
