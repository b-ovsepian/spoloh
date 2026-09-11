package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"time"
)

// socketMode is the whole authorisation model: owner and group may connect,
// nobody else. The owner is the daemon; the group is what an operator adds
// their own account to.
const socketMode fs.FileMode = 0o660

// connDeadline bounds one exchange. A client that connects and then says
// nothing must not hold a daemon goroutine for the life of the process, and
// every real request is one small JSON object written immediately.
const connDeadline = 5 * time.Second

// maxRequest caps what a single request may be. Local callers are trusted to
// be the right people, not to be well behaved: this daemon has to keep
// announcing alerts on a board with 512 MB of RAM, and a decoder reading an
// unbounded body from a socket is the cheapest way to lose that.
const maxRequest = 64 << 10

// dialProbe is how long the "is somebody already listening here?" connect
// gets. It is answered by the kernel from a local socket's backlog, so this
// is a bound on a pathological case, not a latency anyone waits for.
const dialProbe = 2 * time.Second

// ErrAlreadyListening is returned when the socket path already has a live
// daemon behind it. The caller logs it and carries on without a CLI rather
// than treating it as a reason to stop.
var ErrAlreadyListening = errors.New("another daemon is already listening on the control socket")

// Device is everything the socket can ask the daemon to do. cmd/spoloh
// implements it over the engine, the library and the event log; the tests
// here implement it with a recorder.
//
// Play takes only a name: the volume, the priority and the event log entry
// are the engine's business, and a CLI that passed its own would be a second
// playback path — exactly what `play` must not be.
type Device interface {
	Status() Status
	Sounds() ([]string, error)
	Play(sound string) error
	Stop()
	Log(n int) ([]Entry, error)
	Version() Version
}

// Listen binds the control socket at path and publishes it with the right
// permissions.
//
// Two failure modes have to be told apart and they look identical on disk. A
// socket file left by an unclean shutdown is dead and must be replaced; a
// socket file with a live daemon behind it must not be touched, because
// unlinking it would leave that daemon serving an inode no CLI can find
// while this process took over the name.
//
// So: probe first, then publish by rename.
//
//   - The probe is a connect. Only a process with the socket bound can accept
//     one, so a successful dial is proof of life and this returns
//     ErrAlreadyListening. ECONNREFUSED is proof of the opposite: the file
//     exists, the kernel has no listener for it, it is a leftover.
//   - Publishing binds a private name in the same directory, chmods it, and
//     renames it over path. rename(2) is atomic and replaces whatever is
//     there, so nothing here ever unlinks a file it only believes is dead —
//     the check-then-unlink that would make the probe a real race is simply
//     not performed.
//
// What is left of the race is bounded and harmless: two daemons starting in
// the same instant can both pass the probe, and then the second rename wins
// the name while the first keeps serving an unlinked inode until it exits.
// One reachable listener either way, and no third party's socket removed. In
// production it cannot arise at all — systemd starts exactly one instance of
// one unit, and RuntimeDirectory=spoloh means /run/spoloh is created fresh on
// start and removed on stop, so there is normally no leftover to find.
func Listen(path string) (net.Listener, error) {
	// A run outside systemd (a developer's machine, a manual invocation with
	// -socket somewhere else) has no RuntimeDirectory to create the parent.
	// On a real board this finds it already there.
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, err
	}

	if err := probe(path); err != nil {
		return nil, err
	}

	// Named for this process so two daemons cannot collide on the temporary
	// name itself. A leftover from a crashed run with the same pid is
	// removed: nothing can be listening on it, because the only process that
	// could be is this one.
	tmp := fmt.Sprintf("%s.%d", path, os.Getpid())
	if err := os.Remove(tmp); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	ln, err := net.Listen("unix", tmp)
	if err != nil {
		return nil, err
	}
	// Chmod before the rename, not after: a socket is created with
	// 0777 &^ umask, and the usual 022 leaves the group unable to write —
	// which for a unix socket means unable to connect. Doing it here means
	// the name an operator dials never exists with the wrong mode at all,
	// rather than for a window nobody can measure.
	if err := os.Chmod(tmp, socketMode); err != nil {
		ln.Close()
		return nil, err
	}
	if err := os.Rename(tmp, path); err != nil {
		ln.Close()
		return nil, err
	}
	// Go's unix listener unlinks the path it bound on Close. That path is
	// now the temporary name, which the rename already moved, so Close
	// removes nothing — the published socket outlives this process. systemd
	// takes /run/spoloh away with the unit, and any other run replaces the
	// file by the rename above, so nothing accumulates.
	return ln, nil
}

// probe reports whether path already has a daemon behind it. A path that does
// not exist is free; a path that is not a socket is somebody else's file and
// is refused rather than replaced.
func probe(path string) error {
	fi, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return err
	case fi.Mode()&os.ModeSocket == 0:
		return fmt.Errorf("%s exists and is not a socket; refusing to replace it", path)
	}
	c, err := net.DialTimeout("unix", path, dialProbe)
	if err != nil {
		// Refused, or timed out against a backlog nobody is draining.
		// Either way no useful daemon is behind this name.
		return nil
	}
	c.Close()
	return ErrAlreadyListening
}

// Serve answers commands on path until ctx is cancelled.
//
// It returns an error when the socket cannot be created or the accept loop
// fails, and nil on a clean shutdown. Whoever calls it decides what that
// means — cmd/spoloh retries, because a device with no CLI still has to
// announce alerts.
func Serve(ctx context.Context, path string, dev Device, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}
	ln, err := Listen(path)
	if err != nil {
		return err
	}

	// Closing the listener is what unblocks Accept; there is no deadline on
	// a unix listener worth using instead.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			ln.Close()
		case <-done:
			ln.Close()
		}
	}()

	logger.Info("control socket listening", "path", path)
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		// One connection is one short exchange, so it is handled inline
		// rather than in a goroutine per client: the deadline below bounds
		// it, and a CLI that has to wait behind another CLI for a few
		// milliseconds is not a problem this device has.
		serveConn(conn, dev, logger)
	}
}

func serveConn(conn net.Conn, dev Device, logger *slog.Logger) {
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(connDeadline)); err != nil {
		logger.Warn("could not set the control connection deadline", "err", err)
		return
	}

	var req Request
	if err := json.NewDecoder(io.LimitReader(conn, maxRequest)).Decode(&req); err != nil {
		// Answered rather than dropped: a client that sent something this
		// daemon could not read deserves to be told so instead of seeing an
		// empty reply it has to guess at.
		writeResponse(conn, Response{Error: "could not read the request: " + err.Error()}, logger)
		return
	}
	writeResponse(conn, handle(req, dev), logger)
}

func handle(req Request, dev Device) Response {
	switch req.Command {
	case CmdStatus:
		s := dev.Status()
		return Response{Status: &s}
	case CmdSounds:
		sounds, err := dev.Sounds()
		if err != nil {
			return Response{Error: "could not read the sound library: " + err.Error()}
		}
		return Response{Sounds: sounds}
	case CmdPlay:
		if err := dev.Play(req.Sound); err != nil {
			return Response{Error: err.Error()}
		}
		return Response{}
	case CmdStop:
		dev.Stop()
		return Response{}
	case CmdLog:
		entries, err := dev.Log(req.N)
		if err != nil {
			return Response{Error: "could not read the event log: " + err.Error()}
		}
		return Response{Entries: entries}
	case CmdVersion:
		v := dev.Version()
		return Response{Version: &v}
	default:
		return Response{Error: fmt.Sprintf("unknown command %q", req.Command)}
	}
}

func writeResponse(conn net.Conn, resp Response, logger *slog.Logger) {
	if err := json.NewEncoder(conn).Encode(resp); err != nil {
		// The operator's CLI has already gone, or the deadline fired. There
		// is nobody left to tell, so this is a log line and not a failure
		// that propagates into the accept loop.
		logger.Warn("could not answer a control request", "err", err)
	}
}
