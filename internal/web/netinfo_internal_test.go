package web

import "testing"

// reachableMDNSName is unexported, so this test lives in package web rather
// than web_test alongside the rest of netinfo_test.go.
func TestReachableMDNSName(t *testing.T) {
	for _, tc := range []struct {
		name  string
		mdns  string
		addrs []string
		want  string
	}{
		{"name published, device reachable", "spoloh.local", []string{"10.0.0.5"}, "spoloh.local"},
		// This is the finding the fix is for: an avahi socket that exists
		// while no interface is up must not print an address that cannot
		// resolve, directly above the line that says the device is offline.
		{"name published, device unreachable", "spoloh.local", nil, ""},
		{"nothing published, device reachable", "", []string{"10.0.0.5"}, ""},
		{"nothing published, device unreachable", "", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := reachableMDNSName(tc.mdns, tc.addrs)
			if got != tc.want {
				t.Errorf("reachableMDNSName(%q, %v) = %q, want %q", tc.mdns, tc.addrs, got, tc.want)
			}
		})
	}
}
