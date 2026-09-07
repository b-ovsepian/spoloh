package netmgr_test

import (
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/b-ovsepian/spoloh/internal/config"
	"github.com/b-ovsepian/spoloh/internal/configstore"
	"github.com/b-ovsepian/spoloh/internal/netmgr"
)

func TestGeneratedPasswordSatisfiesWPA2(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		pw, err := netmgr.GenerateAPPassword()
		if err != nil {
			t.Fatalf("GenerateAPPassword: %v", err)
		}
		if len(pw) < 8 {
			t.Fatalf("password %q is shorter than WPA2's minimum of 8; the hotspot would refuse connections", pw)
		}
		if seen[pw] {
			t.Fatalf("password %q was generated twice in 200 draws", pw)
		}
		seen[pw] = true
	}
}

// It is read off a screen and typed into a phone, in a corridor.
func TestGeneratedPasswordAvoidsConfusableCharacters(t *testing.T) {
	const confusable = "l1IO0"
	for i := 0; i < 200; i++ {
		pw, err := netmgr.GenerateAPPassword()
		if err != nil {
			t.Fatalf("GenerateAPPassword: %v", err)
		}
		if strings.ContainsAny(pw, confusable) {
			t.Fatalf("password %q contains a confusable character from %q", pw, confusable)
		}
	}
}

func TestEnsureAPPasswordGeneratesAndPersistsWhenEmpty(t *testing.T) {
	store := openTemp(t)
	if got := store.Get().Network.APPassword; got != "" {
		t.Fatalf("fixture already has a password %q, want empty", got)
	}

	pw, err := netmgr.EnsureAPPassword(store)
	if err != nil {
		t.Fatalf("EnsureAPPassword: %v", err)
	}
	if len(pw) < 8 {
		t.Fatalf("generated password %q is shorter than WPA2's minimum of 8", pw)
	}
	if got := store.Get().Network.APPassword; got != pw {
		t.Fatalf("store holds %q after EnsureAPPassword, want the returned password %q", got, pw)
	}
}

func TestEnsureAPPasswordReturnsExistingValueUnchanged(t *testing.T) {
	store := openTemp(t)
	if err := store.Update(func(c *config.Config) error {
		c.Network.APPassword = "already-set-1"
		return nil
	}); err != nil {
		t.Fatalf("seed Update: %v", err)
	}

	pw, err := netmgr.EnsureAPPassword(store)
	if err != nil {
		t.Fatalf("EnsureAPPassword: %v", err)
	}
	if pw != "already-set-1" {
		t.Fatalf("EnsureAPPassword returned %q, want the existing value unchanged", pw)
	}
	if got := store.Get().Network.APPassword; got != "already-set-1" {
		t.Fatalf("store holds %q, want the existing value untouched", got)
	}
}

// Concurrent first-boot callers (e.g. a supervisor tick racing dashboard
// start-up) must converge on a single persisted password rather than each
// overwriting the other's write.
func TestEnsureAPPasswordIsSafeUnderConcurrentCalls(t *testing.T) {
	store := openTemp(t)

	const goroutines = 8
	results := make([]string, goroutines)
	errs := make([]error, goroutines)

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = netmgr.EnsureAPPassword(store)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: EnsureAPPassword: %v", i, err)
		}
	}
	want := results[0]
	if len(want) < 8 {
		t.Fatalf("generated password %q is shorter than WPA2's minimum of 8", want)
	}
	for i, got := range results {
		if got != want {
			t.Fatalf("goroutine %d returned %q, want every caller to converge on %q", i, got, want)
		}
	}
	if got := store.Get().Network.APPassword; got != want {
		t.Fatalf("store holds %q, want the converged password %q", got, want)
	}
}

func openTemp(t *testing.T) *configstore.Store {
	t.Helper()
	s, err := configstore.Open(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s
}
