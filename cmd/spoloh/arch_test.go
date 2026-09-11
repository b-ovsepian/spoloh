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
//
// That alone only checks the three call sites (ldflags_for,<arch>) against a
// hardcoded map; it never looks at which Go symbol ldflags_for's -X actually
// targets, so a typo or rename in main.<symbol> -- on either side -- would
// pass silently and every binary would ship stamped "dev" forever. The
// second half of this test closes that: it extracts <symbol> from the
// Makefile's -X main.<symbol>=$(1) and asserts main.go really declares a
// package-level var by that name.
func TestMakefileBuildsTheExpectedArtifactNames(t *testing.T) {
	want := map[string]string{
		"dist/spoloh-linux-armv6": "linux-armv6",
		"dist/spoloh-linux-armv7": "linux-armv7",
		"dist/spoloh-linux-arm64": "linux-arm64",
	}

	makefile, err := os.ReadFile("../../Makefile")
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	// The stamp and the output path sit either side of the closing quote of
	// the -ldflags argument: ...$(call ldflags_for,linux-armv6)" -o dist/...
	line := regexp.MustCompile(`ldflags_for,([a-z0-9-]+)\)" -o (dist/[a-z0-9-]+)`)
	got := map[string]string{}
	for _, m := range line.FindAllStringSubmatch(string(makefile), -1) {
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

	// ldflags_for's definition, not its call sites, is where the linker is
	// told which Go symbol to write into: -X main.<symbol>=$(1). Extract
	// <symbol> and confirm main.go actually declares it, so a rename or
	// typo on either side of that link fails here instead of shipping
	// binaries that quietly keep their "dev" sentinel forever.
	symbolLine := regexp.MustCompile(`-X main\.(\w+)=\$\(1\)`)
	sm := symbolLine.FindSubmatch(makefile)
	if sm == nil {
		t.Fatalf("Makefile's ldflags_for has no -X main.<symbol>=$(1); the per-target arch stamp is no longer wired to any Go variable")
	}
	symbol := string(sm[1])

	mainGo, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	decl := regexp.MustCompile(`(?m)^var ` + regexp.QuoteMeta(symbol) + `\b`)
	if !decl.Match(mainGo) {
		t.Errorf("Makefile stamps -X main.%s via ldflags_for, but main.go declares no package-level var %s", symbol, symbol)
	}
}
