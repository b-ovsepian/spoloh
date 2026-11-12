package netmgr_test

import (
	"testing"
	"time"

	"github.com/b-ovsepian/spoloh/internal/netmgr"
)

// Not every construction has a dashboard behind it, and "nobody is watching"
// is the only safe reading of that: a nil that read as busy would keep the
// access point up for good and the device off a network it remembers.
func TestANilActivityIsNeverRecent(t *testing.T) {
	var a *netmgr.Activity
	a.Touch(boot) // must not panic
	if a.Within(boot, time.Hour) {
		t.Fatal("a nil Activity reported a recent request")
	}
}

func TestActivityIsRecentOnlyWithinItsWindow(t *testing.T) {
	var a netmgr.Activity
	a.Touch(boot)
	if !a.Within(boot.Add(119*time.Second), 2*time.Minute) {
		t.Fatal("a request 119s ago was not recent within a 2-minute window")
	}
	if a.Within(boot.Add(2*time.Minute), 2*time.Minute) {
		t.Fatal("a request exactly 2 minutes ago was still recent within a 2-minute window")
	}
}
