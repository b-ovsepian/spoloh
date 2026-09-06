// Package watchdog speaks systemd's sd_notify protocol. It is implemented
// directly rather than pulled from a library: the protocol is a handful of
// datagrams, and every candidate library drags in cgo.
package watchdog

import (
	"net"
	"os"
	"strconv"
	"sync"
	"time"
)

// Interval returns how often Alive should be called, and whether the watchdog
// is enabled at all. systemd expects a ping at least twice per configured
// period, so this is half of WATCHDOG_USEC.
func Interval() (time.Duration, bool) {
	raw := os.Getenv("WATCHDOG_USEC")
	if raw == "" {
		return 0, false
	}
	usec, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || usec <= 0 {
		return 0, false
	}
	return time.Duration(usec) * time.Microsecond / 2, true
}

// Notifier sends state messages to systemd. A nil *Notifier is valid and does
// nothing, so callers never have to check whether systemd is present.
type Notifier struct {
	conn     *net.UnixConn
	interval time.Duration

	mu   sync.Mutex
	last time.Time
}

// Open connects to the systemd notify socket. Outside systemd it returns a nil
// notifier and no error.
func Open() (*Notifier, error) {
	addr := os.Getenv("NOTIFY_SOCKET")
	if addr == "" {
		return nil, nil
	}
	// A leading '@' means the Linux abstract namespace.
	if addr[0] == '@' {
		addr = "\x00" + addr[1:]
	}

	conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: addr, Net: "unixgram"})
	if err != nil {
		return nil, err
	}
	interval, ok := Interval()
	if !ok {
		interval = 15 * time.Second
	}
	return &Notifier{conn: conn, interval: interval}, nil
}

// Ready tells systemd that startup is complete.
func (n *Notifier) Ready() { n.send("READY=1") }

// Stopping tells systemd that shutdown has begun.
func (n *Notifier) Stopping() { n.send("STOPPING=1") }

// Alive reports progress. It is safe to call on every loop iteration: sends are
// throttled to the notifier's interval.
func (n *Notifier) Alive() {
	if n == nil {
		return
	}
	n.mu.Lock()
	now := time.Now()
	if !n.last.IsZero() && now.Sub(n.last) < n.interval {
		n.mu.Unlock()
		return
	}
	n.last = now
	n.mu.Unlock()

	n.send("WATCHDOG=1")
}

func (n *Notifier) send(msg string) {
	if n == nil || n.conn == nil {
		return
	}
	// A failed notification must never take the daemon down; systemd will
	// restart us on its own if the pings stop.
	_, _ = n.conn.Write([]byte(msg))
}

// Close releases the notify socket.
func (n *Notifier) Close() error {
	if n == nil || n.conn == nil {
		return nil
	}
	return n.conn.Close()
}
