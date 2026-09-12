package main

import (
	"os"
	"regexp"
	"strings"
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

// Every Deps field the dashboard needs has to be filled here, and the
// compiler will not say so: web.Deps is a struct literal with named fields,
// so a missing one is the zero value, not a build error.
//
// Version is the field that proves the point. It shipped unwired: the footer
// on a real board read «збірка для розробки» while running a stamped
// release, which is the one line an operator reads to answer "what is this
// box actually running" before reporting a fault. Nothing failed. Nothing
// could — internal/web's own tests construct their own Deps.
//
// So this reads main.go's own source, the same technique the Makefile test
// above uses for -X symbols, and fails if the literal stops carrying the
// fields whose absence is silent rather than loud.
func TestWebDepsLiteralCarriesTheSilentlyOptionalFields(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}

	literal := regexp.MustCompile(`(?s)web\.New\(web\.Deps\{(.*?)\n\t\}\)`).FindSubmatch(src)
	if literal == nil {
		t.Fatal("could not find the web.New(web.Deps{...}) literal in main.go")
	}
	body := string(literal[1])

	// Only fields whose absence produces a wrong page rather than a crash.
	// A nil Library or Player is guarded for and degrades visibly; a zero
	// Version renders as a confident lie.
	for _, field := range []string{"Version:"} {
		if !regexp.MustCompile(`\b` + field).MatchString(body) {
			t.Errorf("web.Deps literal in main.go does not set %s; "+
				"its zero value renders as if it were true", field)
		}
	}
}

// The webhook pair is only a pair if the second half lands. A before webhook
// that muted a music player feeding these speakers has an after webhook that
// unmutes it, and the after fires from the player's shutdown — so the process
// must outlive the request it just started, or the mute survives the daemon
// that asked for it.
//
// Read from main.go's own source: the ordering is between two calls in one
// function and there is nothing to observe from outside it. Same technique as
// the Makefile -X check above, for the same reason.
func TestShutdownDrainsWebhooksAfterThePlayerStops(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	body := string(src)

	players := strings.Index(body, "players.Wait()")
	hooks := strings.Index(body, "hooks.Wait()")

	if players < 0 {
		t.Fatal("main.go no longer waits for the player goroutines")
	}
	if hooks < 0 {
		t.Fatal("main.go does not drain in-flight webhooks on shutdown; " +
			"an after webhook started by the player falling silent would never land")
	}
	if hooks < players {
		t.Error("webhooks are drained before the player stops, so the after webhook " +
			"the player's own shutdown fires is not among the ones waited for")
	}
}
