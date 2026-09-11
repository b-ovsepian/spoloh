package main

import (
	"os"
	"regexp"
	"testing"
)

// The Makefile, the updater and install.sh all build the same artifact names
// from different languages. Nothing else makes them agree, so this test reads
// the Makefile and fails if the set of names it produces ever changes without
// this list changing with it.
func TestMakefileBuildsTheExpectedArtifactNames(t *testing.T) {
	want := map[string]string{
		"dist/spoloh-linux-armv6": "linux-armv6",
		"dist/spoloh-linux-armv7": "linux-armv7",
		"dist/spoloh-linux-arm64": "linux-arm64",
	}

	b, err := os.ReadFile("../../Makefile")
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	// The stamp and the output path sit either side of the closing quote of
	// the -ldflags argument: ...$(call ldflags_for,linux-armv6)" -o dist/...
	line := regexp.MustCompile(`ldflags_for,([a-z0-9-]+)\)" -o (dist/[a-z0-9-]+)`)
	got := map[string]string{}
	for _, m := range line.FindAllStringSubmatch(string(b), -1) {
		got[m[2]] = m[1]
	}
	if len(got) != len(want) {
		t.Fatalf("Makefile builds %d artifacts, want %d: %v", len(got), len(want), got)
	}
	for path, stamp := range want {
		if got[path] != stamp {
			t.Errorf("%s is stamped %q, want %q", path, got[path], stamp)
		}
	}
}
