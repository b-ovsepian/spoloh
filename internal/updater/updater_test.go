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
	"syscall"
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

func TestLatestRefusesARedirectThatDoesNotLandOnATagPage(t *testing.T) {
	// Neither case installs anything unsafe by itself (the bogus URL still
	// 404s or fails its checksum, and Apply refuses either way), but both
	// would report "download failed: 404" instead of naming the real cause,
	// and on a headless board the error message is the entire diagnostic
	// surface.
	cases := map[string]struct {
		location string
		extra    map[string]stubPage
	}{
		// A login page is the realistic case a redirect could land on
		// instead of a release.
		"redirect lands on an unrelated page": {
			location: base + "/login",
			extra:    map[string]stubPage{base + "/login": {status: 200, body: "login page"}},
		},
		// A trailing slash with no version after it: path.Base alone would
		// read this as tag "tag" (Base drops the empty final path element),
		// which is exactly the bug this test exists to catch — the segment
		// before the version must be checked after the trailing slash is
		// accounted for, not before.
		"redirect lands on the tag index with no version": {
			location: base + "/tag/",
			extra:    map[string]stubPage{base + "/tag/": {status: 200, body: "release index"}},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			pages := releasePages(binaryBody)
			pages[base+"/latest"] = stubPage{status: 302, location: tc.location}
			for url, page := range tc.extra {
				pages[url] = page
			}
			u, _, _ := newUpdater(t, pages)

			_, err := u.Latest(context.Background())
			if err == nil {
				t.Fatal("Latest accepted a redirect that did not land on a tag page")
			}
		})
	}
}

func TestLatestAcceptsAVersionWithoutALeadingV(t *testing.T) {
	// The redirect-shape check must not also guess at the version string's
	// own format: a project may tag "1.2.3" as readily as "v1.2.3", and
	// rejecting one would be a guess about someone else's conventions.
	pages := map[string]stubPage{
		base + "/latest":                            {status: 302, location: base + "/tag/1.2.3"},
		base + "/tag/1.2.3":                         {status: 200, body: "release page"},
		base + "/download/1.2.3/spoloh-linux-armv7": {status: 200, body: binaryBody},
		base + "/download/1.2.3/SHA256SUMS":         {status: 200, body: sum(binaryBody) + "  spoloh-linux-armv7\n"},
	}
	u, _, _ := newUpdater(t, pages)

	rel, err := u.Latest(context.Background())
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if rel.Version != "1.2.3" {
		t.Errorf("Version = %q, want 1.2.3", rel.Version)
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

// TestApplyAndRollbackIgnoreTheInvokingUmask covers the one path that can
// brick a device: Rollback is the mechanism that exists to rescue one.
//
// Apply chmods the downloaded binary explicitly, but the rollback snapshot
// and the restore both go through copyFile, which passed 0o755 to OpenFile —
// a request, not a setting, since the umask masks it. sudo does not reset a
// user's umask, so a volunteer with umask 077 in their shell got a .previous
// at -rwx------ and, after a rollback, a /usr/local/bin/spoloh at -rwx------,
// which User=spoloh in the unit cannot execute. The device would then be
// down with no binary anyone can run and no way in but the SD card.
func TestApplyAndRollbackIgnoreTheInvokingUmask(t *testing.T) {
	old := syscall.Umask(0o077)
	t.Cleanup(func() { syscall.Umask(old) })

	u, binPath, _ := newUpdater(t, releasePages(binaryBody))
	rel, err := u.Latest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := u.Apply(context.Background(), rel); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	mustBeExecutable(t, binPath+".previous", "the rollback snapshot")

	if err := u.Rollback(context.Background()); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	mustBeExecutable(t, binPath, "the restored binary")
}

func mustBeExecutable(t *testing.T, path, what string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	if mode := info.Mode().Perm(); mode != 0o755 {
		t.Errorf("%s is %v, want -rwxr-xr-x: the unit runs as User=spoloh and cannot execute it", what, mode)
	}
}

func TestApplyRefusesABinaryThatReportsADifferentVersion(t *testing.T) {
	// A release built from something other than its tag passes every other
	// check here: the checksum is of whatever was published, and the binary
	// runs. Installing it leaves the device reporting the old version, so
	// the next run downloads and applies the same release again — silently,
	// forever, over a school's connection.
	mistagged := "#!/bin/sh\necho v1.0.0\n"
	u, binPath, ran := newUpdater(t, releasePages(mistagged))
	rel, _ := u.Latest(context.Background())

	err := u.Apply(context.Background(), rel)
	if err == nil {
		t.Fatal("Apply installed a binary that does not report the version of the release it came from")
	}
	if !strings.Contains(err.Error(), "v1.0.0") || !strings.Contains(err.Error(), "v9.9.9") {
		t.Errorf("err = %v, want it to name both versions", err)
	}
	if got, _ := os.ReadFile(binPath); !strings.Contains(string(got), "v1.0.0") {
		t.Errorf("the installed binary was replaced anyway: %q", got)
	}
	if len(*ran) != 0 {
		t.Errorf("commands were run despite the refusal: %v", *ran)
	}
}

func TestApplySweepsWhatAnInterruptedRunLeftBehind(t *testing.T) {
	// Go runs no deferred function when the process is killed by a signal,
	// so an interrupted update leaves its download — tens of megabytes —
	// beside the binary, on an SD card the spec says wears out.
	u, binPath, _ := newUpdater(t, releasePages(binaryBody))
	dir := filepath.Dir(binPath)
	stale := filepath.Join(dir, ".spoloh-update-2360015")
	if err := os.WriteFile(stale, []byte("an interrupted download"), 0o644); err != nil {
		t.Fatal(err)
	}
	rel, _ := u.Latest(context.Background())

	if err := u.Apply(context.Background(), rel); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("an interrupted run's download survived: stat = %v", err)
	}
}
