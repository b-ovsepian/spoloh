package watchdog_test

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/b-ovsepian/spoloh/internal/watchdog"
)

func TestIntervalIsHalfTheSystemdPeriod(t *testing.T) {
	t.Setenv("WATCHDOG_USEC", "30000000") // 30 s
	got, ok := watchdog.Interval()
	if !ok {
		t.Fatal("expected the watchdog to be enabled")
	}
	if got != 15*time.Second {
		t.Fatalf("interval = %v, want 15s (half of 30s)", got)
	}
}

func TestIntervalDisabled(t *testing.T) {
	for _, v := range []string{"", "0", "not-a-number"} {
		t.Run("value="+v, func(t *testing.T) {
			t.Setenv("WATCHDOG_USEC", v)
			if _, ok := watchdog.Interval(); ok {
				t.Fatalf("watchdog should be disabled for %q", v)
			}
		})
	}
}

func TestOpenWithoutSystemdReturnsNilNotifier(t *testing.T) {
	t.Setenv("NOTIFY_SOCKET", "")
	n, err := watchdog.Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if n != nil {
		t.Fatal("Open must return a nil notifier outside systemd")
	}
	// Every method must be safe on the nil notifier, so callers never branch.
	n.Ready()
	n.Alive()
	n.Stopping()
	if err := n.Close(); err != nil {
		t.Fatalf("Close on a nil notifier: %v", err)
	}
}

func TestNotifierSendsReadyAndKeepalive(t *testing.T) {
	// Keep the path short: a unix socket path is capped near 104 bytes, and the
	// per-test temp directory on macOS is long enough to overflow it.
	dir, err := os.MkdirTemp("/tmp", "spwd")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	defer os.RemoveAll(dir)

	sock := filepath.Join(dir, "n.sock")
	addr, err := net.ResolveUnixAddr("unixgram", sock)
	if err != nil {
		t.Fatalf("ResolveUnixAddr: %v", err)
	}
	conn, err := net.ListenUnixgram("unixgram", addr)
	if err != nil {
		t.Fatalf("ListenUnixgram: %v", err)
	}
	defer conn.Close()

	t.Setenv("NOTIFY_SOCKET", sock)
	t.Setenv("WATCHDOG_USEC", "30000000")

	n, err := watchdog.Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if n == nil {
		t.Fatal("expected a live notifier")
	}
	defer n.Close()

	n.Ready()
	if got := readMsg(t, conn); got != "READY=1" {
		t.Fatalf("first message = %q, want READY=1", got)
	}

	n.Alive()
	if got := readMsg(t, conn); got != "WATCHDOG=1" {
		t.Fatalf("second message = %q, want WATCHDOG=1", got)
	}
}

// Pinging on every one-second tick would flood the socket; the notifier must
// throttle itself to its own interval.
func TestAliveIsThrottled(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "spwd")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	defer os.RemoveAll(dir)

	sock := filepath.Join(dir, "n.sock")
	addr, _ := net.ResolveUnixAddr("unixgram", sock)
	conn, err := net.ListenUnixgram("unixgram", addr)
	if err != nil {
		t.Fatalf("ListenUnixgram: %v", err)
	}
	defer conn.Close()

	t.Setenv("NOTIFY_SOCKET", sock)
	t.Setenv("WATCHDOG_USEC", "30000000") // ping at most every 15 s

	n, err := watchdog.Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer n.Close()

	for i := 0; i < 10; i++ {
		n.Alive()
	}
	if got := readMsg(t, conn); got != "WATCHDOG=1" {
		t.Fatalf("message = %q, want WATCHDOG=1", got)
	}
	conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	buf := make([]byte, 64)
	if _, err := conn.Read(buf); err == nil {
		t.Fatalf("a second ping got through: %q", buf)
	}
}

func readMsg(t *testing.T, conn *net.UnixConn) string {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 64)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	return string(buf[:n])
}
