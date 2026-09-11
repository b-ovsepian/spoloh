package updater_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/b-ovsepian/spoloh/internal/updater"
)

// stubTransport serves a fixed map of URLs. No socket is opened: the
// constraint is that tests never reach the network, and a loopback server
// would still be one.
type stubTransport struct {
	pages map[string]stubPage
	hits  []string
}

type stubPage struct {
	status   int
	body     string
	location string
}

func (s *stubTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	s.hits = append(s.hits, r.URL.String())
	p, ok := s.pages[r.URL.String()]
	if !ok {
		return &http.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader("not found")), Header: http.Header{}, Request: r}, nil
	}
	h := http.Header{}
	if p.location != "" {
		h.Set("Location", p.location)
	}
	return &http.Response{StatusCode: p.status, Body: io.NopCloser(strings.NewReader(p.body)), Header: h, Request: r}, nil
}

const base = "https://example.invalid/releases"

func sum(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// binaryBody is a shell script rather than a real ELF: Apply proves the
// download runs by executing it with -version, and a script satisfies that
// on the machine the tests run on.
const binaryBody = "#!/bin/sh\necho v9.9.9\n"

func newUpdater(t *testing.T, pages map[string]stubPage) (updater.Updater, string, *[]string) {
	t.Helper()
	dir := t.TempDir()
	binPath := filepath.Join(dir, "spoloh")
	if err := os.WriteFile(binPath, []byte("#!/bin/sh\necho v1.0.0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	var ran []string
	u := updater.Updater{
		BaseURL: base,
		Arch:    "linux-armv7",
		Version: "v1.0.0",
		BinPath: binPath,
		Unit:    "spoloh",
		HTTP:    &http.Client{Transport: &stubTransport{pages: pages}},
		Run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			ran = append(ran, name+" "+strings.Join(args, " "))
			return nil, nil
		},
	}
	return u, binPath, &ran
}

func releasePages(binary string) map[string]stubPage {
	checks := fmt.Sprintf("%s  spoloh-linux-armv7\n%s  spoloh-linux-arm64\n", sum(binary), sum("other"))
	return map[string]stubPage{
		base + "/latest":                             {status: 302, location: base + "/tag/v9.9.9"},
		base + "/tag/v9.9.9":                         {status: 200, body: "release page"},
		base + "/download/v9.9.9/spoloh-linux-armv7": {status: 200, body: binary},
		base + "/download/v9.9.9/SHA256SUMS":         {status: 200, body: checks},
	}
}

func TestLatestReadsTheTagFromTheRedirect(t *testing.T) {
	// GitHub redirects /releases/latest to /releases/tag/<tag>. Reading the
	// redirect avoids the API entirely: no token, no rate limit, no JSON.
	u, _, _ := newUpdater(t, releasePages(binaryBody))

	rel, err := u.Latest(context.Background())
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if rel.Version != "v9.9.9" {
		t.Errorf("Version = %q, want v9.9.9", rel.Version)
	}
	if want := base + "/download/v9.9.9/spoloh-linux-armv7"; rel.BinaryURL != want {
		t.Errorf("BinaryURL = %q, want %q", rel.BinaryURL, want)
	}
}

func TestApplyInstallsAndKeepsThePreviousBinary(t *testing.T) {
	u, binPath, ran := newUpdater(t, releasePages(binaryBody))
	rel, err := u.Latest(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if err := u.Apply(context.Background(), rel); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	got, _ := os.ReadFile(binPath)
	if string(got) != binaryBody {
		t.Errorf("binary was not replaced: %q", got)
	}
	prev, err := os.ReadFile(binPath + ".previous")
	if err != nil {
		t.Fatalf("no rollback copy was kept: %v", err)
	}
	if !strings.Contains(string(prev), "v1.0.0") {
		t.Errorf("rollback copy holds %q, want the version that was running", prev)
	}
	if len(*ran) == 0 || !strings.Contains((*ran)[len(*ran)-1], "restart spoloh") {
		t.Errorf("commands run = %v, want the unit restarted last", *ran)
	}
}

func TestApplyRefusesAChecksumMismatchAndChangesNothing(t *testing.T) {
	// The binary served does not match the checksum published for it. This is
	// the case that must never install: a truncated download would otherwise
	// leave a school with a device that cannot start.
	pages := releasePages(binaryBody)
	pages[base+"/download/v9.9.9/spoloh-linux-armv7"] = stubPage{status: 200, body: "#!/bin/sh\necho tampered\n"}
	u, binPath, ran := newUpdater(t, pages)
	rel, _ := u.Latest(context.Background())

	err := u.Apply(context.Background(), rel)
	if err == nil {
		t.Fatal("Apply accepted a binary whose checksum did not match")
	}
	if !strings.Contains(err.Error(), "checksum") {
		t.Errorf("err = %v, want it to name the checksum", err)
	}
	got, _ := os.ReadFile(binPath)
	if !strings.Contains(string(got), "v1.0.0") {
		t.Errorf("the installed binary was touched: %q", got)
	}
	if len(*ran) != 0 {
		t.Errorf("commands were run despite the refusal: %v", *ran)
	}
	if entries, _ := filepath.Glob(binPath + "*"); len(entries) != 1 {
		t.Errorf("left files behind: %v", entries)
	}
}

func TestApplyRefusesWhenTheChecksumFileHasNoLineForThisArch(t *testing.T) {
	pages := releasePages(binaryBody)
	pages[base+"/download/v9.9.9/SHA256SUMS"] = stubPage{status: 200, body: sum("x") + "  spoloh-linux-arm64\n"}
	u, _, _ := newUpdater(t, pages)
	rel, _ := u.Latest(context.Background())

	err := u.Apply(context.Background(), rel)
	if err == nil || !strings.Contains(err.Error(), "linux-armv7") {
		t.Fatalf("err = %v, want a refusal naming the missing artifact", err)
	}
}

func TestApplyRefusesABinaryThatWillNotRun(t *testing.T) {
	// Right checksum, wrong architecture: exactly what a mis-tagged release
	// looks like, and the one failure a checksum cannot catch.
	broken := "\x7fELF not really\n"
	u, binPath, _ := newUpdater(t, releasePages(broken))
	rel, _ := u.Latest(context.Background())

	err := u.Apply(context.Background(), rel)
	if err == nil {
		t.Fatal("Apply installed a binary it could not execute")
	}
	got, _ := os.ReadFile(binPath)
	if !strings.Contains(string(got), "v1.0.0") {
		t.Errorf("the installed binary was replaced anyway: %q", got)
	}
}

func TestApplyRollsBackWhenTheUnitWillNotRestart(t *testing.T) {
	u, binPath, ran := newUpdater(t, releasePages(binaryBody))
	// Counted, not inferred from position: Apply runs the downloaded binary
	// with -version before it ever reaches systemctl, so "the first command"
	// is not the first restart.
	restarts := 0
	u.Run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		*ran = append(*ran, name+" "+strings.Join(args, " "))
		if len(args) > 0 && args[0] == "restart" {
			restarts++
			if restarts == 1 {
				return []byte("Job for spoloh.service failed"), errors.New("exit status 1")
			}
		}
		return nil, nil
	}
	rel, _ := u.Latest(context.Background())

	err := u.Apply(context.Background(), rel)
	if err == nil {
		t.Fatal("Apply reported success after the unit failed to start")
	}
	got, _ := os.ReadFile(binPath)
	if !strings.Contains(string(got), "v1.0.0") {
		t.Errorf("binary after a failed restart is %q, want the previous one restored", got)
	}
	if len(*ran) < 2 {
		t.Errorf("commands run = %v, want a second restart after the rollback", *ran)
	}
}

func TestApplyRefusesWhenTheBinaryWasNeverPublished(t *testing.T) {
	// A locally built binary has no artifact to be an update of. Updating one
	// would replace an operator's own build with a release.
	u, _, _ := newUpdater(t, releasePages(binaryBody))
	u.Arch = "dev"

	_, err := u.Latest(context.Background())
	if err == nil || !strings.Contains(err.Error(), "dev") {
		t.Fatalf("err = %v, want a refusal naming the unpublished build", err)
	}
}
