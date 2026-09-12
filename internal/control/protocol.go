// Package control is the device's command line: a unix socket the daemon
// listens on and the `spoloh` binary talks to when it is given a verb.
//
// A socket rather than HTTP, deliberately. The dashboard is password
// protected, so a CLI built on it would need a token — a second credential to
// mint, store and rotate for a job the filesystem already does. Socket
// permissions are the whole authorisation model here: 0660, owned
// spoloh:spoloh, so whoever is in the spoloh group can drive the device and
// nobody else can. A unix socket also cannot be reached from the network at
// all, which is the right blast radius for "sound the siren".
//
// One JSON request per connection, one JSON response, then close. Nothing
// here needs a stream, and a protocol that can be exercised with one Encode
// and one Decode is a protocol that can be read from a test.
package control

import "time"

// DefaultSocket is where deploy/spoloh.service's RuntimeDirectory=spoloh puts
// it. systemd creates /run/spoloh owned by the unit's User=/Group= before
// ExecStart and removes it when the unit stops, which is also why a socket
// left behind by an unclean shutdown is rare rather than routine.
const DefaultSocket = "/run/spoloh/control.sock"

// The verbs. They are constants because main dispatches on them before
// flag.Parse and the client and server both have to agree on the spelling.
const (
	CmdStatus  = "status"
	CmdSounds  = "sounds"
	CmdPlay    = "play"
	CmdStop    = "stop"
	CmdLog     = "log"
	CmdVersion = "version"
)

// IsCommand reports whether s is one of this package's verbs. cmd/spoloh asks
// before it hands anything over, so an unknown word still gets main's own
// "unknown command" message rather than a connection attempt.
func IsCommand(s string) bool {
	switch s {
	case CmdStatus, CmdSounds, CmdPlay, CmdStop, CmdLog, CmdVersion:
		return true
	}
	return false
}

// Request is one command. Sound is read by play and N by log; the rest ignore
// them.
type Request struct {
	Command string `json:"command"`
	Sound   string `json:"sound,omitempty"`
	N       int    `json:"n,omitempty"`
}

// Response carries whichever field the command it answers fills in. One
// struct rather than one type per verb: the client decodes without knowing
// what it asked for, and an empty field is indistinguishable from an absent
// one for every command here.
//
// Error being non-empty means the request failed and no other field is
// meaningful. It is a string and not an error because it crosses JSON.
type Response struct {
	Error   string   `json:"error,omitempty"`
	Status  *Status  `json:"status,omitempty"`
	Sounds  []string `json:"sounds,omitempty"`
	Entries []Entry  `json:"entries,omitempty"`
	Version *Version `json:"version,omitempty"`
}

// Status is what `spoloh status` prints. It is this package's own type rather
// than engine.Status because it answers a different question: the engine's is
// a view model for the dashboard's templates, and half of it (the next
// scheduled event, the presentation layer's region) is filled in by whoever
// renders it.
//
// Times keep the location the daemon put them in. JSON carries the offset, so
// a client formatting with RFC3339 prints the site's local time without
// having to know the configured zone.
type Status struct {
	Alert      bool      `json:"alert"`
	Since      time.Time `json:"since"`
	Region     string    `json:"region"`
	Source     string    `json:"source"`
	LastUpdate time.Time `json:"last_update"`
	// PollPaused is true while the device is serving its own access point
	// with no upstream and has therefore stopped asking about raids. It sits
	// beside LastUpdate because it is what makes that line readable: a last
	// update forty minutes old means one thing on a device that cannot reach
	// its sources and something else entirely on a device that has decided
	// not to try. Without it the two are the same line, and one of them is a
	// fault.
	PollPaused  bool   `json:"poll_paused"`
	SourceError string `json:"source_error,omitempty"`
	ClockSynced bool   `json:"clock_synced"`
	Output      string `json:"output"`
	// Playing is the sound on the speaker right now, bare name, empty when
	// the device is quiet.
	Playing string `json:"playing,omitempty"`
}

// Entry is one event log row, flattened for printing.
type Entry struct {
	At    time.Time `json:"at"`
	Kind  string    `json:"kind"`
	Event string    `json:"event,omitempty"`
	Sound string    `json:"sound,omitempty"`
	OK    bool      `json:"ok"`
	Err   string    `json:"err,omitempty"`
}

// Version is the running daemon's, not the binary the operator just typed.
// The two differ for the whole window between `spoloh update` replacing
// /usr/local/bin/spoloh and the unit restarting, and which one is on the
// speaker is the question worth answering. `spoloh -version` still reports
// the local binary and needs no daemon.
type Version struct {
	Version string `json:"version"`
	Arch    string `json:"arch"`
}
