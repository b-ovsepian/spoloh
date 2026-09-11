// Package updater replaces this device's binary with a published release.
//
// It is the most dangerous code in the product: it overwrites the executable
// of a device a school relies on to hear an air raid warning. Every step is
// ordered so that a failure at any point leaves a working binary in place —
// the download is verified before anything is moved, the replacement is
// proven to execute before it is installed, the previous binary is kept, and
// a unit that will not come up is rolled back without anyone being asked.
package updater

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// downloadTimeout bounds one fetch. A school's connection is often slow and
// sometimes a captive portal, so this is generous; it exists to stop a hung
// TCP connection holding the command forever, not to hurry anyone.
const downloadTimeout = 5 * time.Minute

// maxDownload caps what will be read from the network. The binaries are a few
// megabytes; anything approaching this is a redirect to something that is not
// a release, and reading it to the end would exhaust a Pi's memory.
const maxDownload = 64 << 20

// Release is one published version and the two files this device needs.
type Release struct {
	Version     string
	BinaryURL   string
	ChecksumURL string
}

// RunFunc executes a command. Replaced in tests; nil means the real thing.
type RunFunc func(ctx context.Context, name string, args ...string) ([]byte, error)

type Updater struct {
	// BaseURL is the releases root, e.g.
	// https://github.com/b-ovsepian/spoloh/releases
	BaseURL string
	// Arch is the artifact name suffix this binary was built as, stamped at
	// build time. "dev" means a local build and is refused.
	Arch string
	// Version is what is running now.
	Version string
	// BinPath is the binary to replace.
	BinPath string
	// Unit is the systemd unit to restart.
	Unit string

	HTTP   *http.Client
	Run    RunFunc
	Logger *slog.Logger
}

func (u Updater) logger() *slog.Logger {
	if u.Logger != nil {
		return u.Logger
	}
	return slog.Default()
}

func (u Updater) client() *http.Client {
	if u.HTTP != nil {
		return u.HTTP
	}
	return &http.Client{Timeout: downloadTimeout}
}

func (u Updater) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	if u.Run != nil {
		return u.Run(ctx, name, args...)
	}
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// Latest resolves the newest published release.
//
// It reads the tag out of the redirect that /releases/latest performs rather
// than calling the GitHub API: the API needs a token to avoid a rate limit
// that a classroom full of devices behind one address would hit, and returns
// JSON this package would then have to model. The redirect needs nothing.
func (u Updater) Latest(ctx context.Context) (Release, error) {
	if u.Arch == "" || u.Arch == "dev" {
		return Release{}, fmt.Errorf("this binary was built locally (arch %q), not installed from a release; there is nothing to update it to", u.Arch)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.BaseURL+"/latest", nil)
	if err != nil {
		return Release{}, err
	}
	resp, err := u.client().Do(req)
	if err != nil {
		return Release{}, fmt.Errorf("ask for the latest release: %w", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, maxDownload))

	if resp.StatusCode != http.StatusOK {
		return Release{}, fmt.Errorf("ask for the latest release: %s", resp.Status)
	}
	// resp.Request is the last request the client made, so this is where the
	// redirect landed.
	tag := path.Base(resp.Request.URL.Path)
	if tag == "" || tag == "." || tag == "/" || tag == "latest" {
		return Release{}, fmt.Errorf("could not read a version from %q", resp.Request.URL)
	}
	return Release{
		Version:     tag,
		BinaryURL:   fmt.Sprintf("%s/download/%s/spoloh-%s", u.BaseURL, tag, u.Arch),
		ChecksumURL: fmt.Sprintf("%s/download/%s/SHA256SUMS", u.BaseURL, tag),
	}, nil
}

// Apply installs rel, or changes nothing and returns why.
func (u Updater) Apply(ctx context.Context, rel Release) error {
	want, err := u.wantedSum(ctx, rel)
	if err != nil {
		return err
	}

	// Downloaded into the directory the binary already lives in, because the
	// install is a rename and a rename is only atomic within one filesystem.
	dir := filepath.Dir(u.BinPath)
	tmp, err := os.CreateTemp(dir, ".spoloh-update-*")
	if err != nil {
		return fmt.Errorf("make room for the download: %w", err)
	}
	tmpName := tmp.Name()
	// Removed on every path out; the rename below makes this a no-op on the
	// path where it succeeded.
	defer os.Remove(tmpName)

	got, err := u.download(ctx, rel.BinaryURL, tmp)
	tmp.Close()
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("checksum of %s does not match the published one (got %s, want %s); nothing was changed", rel.BinaryURL, got, want)
	}
	if err := os.Chmod(tmpName, 0o755); err != nil {
		return fmt.Errorf("make the download executable: %w", err)
	}

	// The checksum proves the file arrived intact; it cannot prove the file is
	// for this board. A mis-tagged release, or an Arch stamp that is wrong,
	// both produce a perfectly intact binary this CPU cannot run — and
	// installing one is how a device stops announcing.
	//
	// This always shells out for real, rather than going through Run: Run
	// exists so tests can avoid running systemctl on a machine that has no
	// spoloh unit, but whether the downloaded file can execute is a property
	// of this machine's CPU, and faking that would defeat the one check that
	// exists to catch a mis-tagged release. In production Run is nil, so
	// u.run already resolves to exec.CommandContext for every call; this only
	// changes what a test that injects Run can observe.
	out, err := exec.CommandContext(ctx, tmpName, "-version").CombinedOutput()
	if err != nil {
		return fmt.Errorf("the downloaded binary does not run on this board (%w); nothing was changed: %s", err, strings.TrimSpace(string(out)))
	}
	u.logger().Info("downloaded release verified", "version", rel.Version, "reports", strings.TrimSpace(string(out)))

	// Copied rather than renamed: renaming would leave an instant with no
	// binary at BinPath at all, and this runs on a device that can lose power
	// at any moment.
	if err := copyFile(u.BinPath, u.BinPath+".previous"); err != nil {
		return fmt.Errorf("keep a copy to roll back to: %w; nothing was changed", err)
	}
	if err := os.Rename(tmpName, u.BinPath); err != nil {
		return fmt.Errorf("install the new binary: %w", err)
	}

	if out, err := u.run(ctx, "systemctl", "restart", u.Unit); err != nil {
		u.logger().Error("the new version would not start, rolling back", "err", err, "output", strings.TrimSpace(string(out)))
		if rbErr := u.Rollback(ctx); rbErr != nil {
			return fmt.Errorf("the new version would not start (%w) and the rollback also failed: %v", err, rbErr)
		}
		return fmt.Errorf("the new version would not start (%w); rolled back to the previous one", err)
	}
	u.logger().Info("updated", "from", u.Version, "to", rel.Version)
	return nil
}

// Rollback puts the previous binary back and restarts the unit.
func (u Updater) Rollback(ctx context.Context) error {
	prev := u.BinPath + ".previous"
	if _, err := os.Stat(prev); err != nil {
		return fmt.Errorf("no previous binary to roll back to: %w", err)
	}
	if err := copyFile(prev, u.BinPath); err != nil {
		return fmt.Errorf("restore the previous binary: %w", err)
	}
	if out, err := u.run(ctx, "systemctl", "restart", u.Unit); err != nil {
		return fmt.Errorf("restart after rollback: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// wantedSum fetches SHA256SUMS and returns the digest published for this
// device's artifact.
func (u Updater) wantedSum(ctx context.Context, rel Release) (string, error) {
	var buf strings.Builder
	if _, err := u.download(ctx, rel.ChecksumURL, &buf); err != nil {
		return "", err
	}
	name := "spoloh-" + u.Arch
	for _, line := range strings.Split(buf.String(), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == name {
			return strings.ToLower(fields[0]), nil
		}
	}
	return "", fmt.Errorf("release %s publishes no checksum for %s; this board's artifact is not in it", rel.Version, name)
}

// download copies url into w and returns what it hashed to.
func (u Updater) download(ctx context.Context, url string, w io.Writer) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := u.client().Do(req)
	if err != nil {
		return "", fmt.Errorf("download %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download %s: %s", url, resp.Status)
	}
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(w, h), io.LimitReader(resp.Body, maxDownload)); err != nil {
		return "", fmt.Errorf("download %s: %w", url, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func copyFile(from, to string) error {
	src, err := os.Open(from)
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := os.OpenFile(to+".tmp", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		os.Remove(to + ".tmp")
		return err
	}
	if err := dst.Sync(); err != nil {
		dst.Close()
		os.Remove(to + ".tmp")
		return err
	}
	if err := dst.Close(); err != nil {
		os.Remove(to + ".tmp")
		return err
	}
	return os.Rename(to+".tmp", to)
}
