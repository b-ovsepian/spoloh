// Package netmgr wraps NetworkManager. It shells out to nmcli rather than
// linking a library, for the same reason the audio and clock layers do: the
// project builds with CGO_ENABLED=0 so one laptop can cross-compile for every
// board, and every candidate binding needs cgo.
package netmgr

import "context"

// Network is one Wi-Fi network as an operator would choose it from a list.
type Network struct {
	SSID    string
	Signal  int // 0..100
	Secured bool
}

// Status is what the device's networking looks like right now.
//
// There is no address field here on purpose. internal/web.LocalAddresses
// already answers "what IP does this device have" by reading Go's own net
// package; it needs no NetworkManager and keeps working even when nmcli is
// broken, so this type does not duplicate it.
type Status struct {
	// Connected means joined to an upstream network — not merely that the
	// wifi device is active, and not only over Wi-Fi: a board on a wall port
	// in a PA closet is joined to one just as much as a board on the site
	// Wi-Fi is. It is specifically false while the device is only serving
	// its own hotspot, because that is the distinction a caller deciding
	// whether to keep the hotspot up actually needs.
	Connected bool
	// SSID is the Wi-Fi network the device is joined to, and is empty when it
	// is connected some other way. A wired connection has no SSID, and its
	// NetworkManager profile name ("Wired connection 1") is not one.
	SSID string
	// HotspotUp means a wifi device is serving this daemon's own access
	// point. It is independent of Connected: a board with a wired upstream
	// can report both, which is exactly when the supervisor lowers the
	// access point.
	HotspotUp bool
}

// Manager is the surface the rest of the daemon uses. Everything above it is
// tested against Fake; only NMCLI ever runs a command.
type Manager interface {
	Status(ctx context.Context) (Status, error)
	Scan(ctx context.Context) ([]Network, error)
	// Rescan sweeps the air first. Slower than Scan and not for a page
	// load; see NMCLI.Rescan for why the two are separate.
	Rescan(ctx context.Context) ([]Network, error)
	Connect(ctx context.Context, ssid, psk string) error
	StartHotspot(ctx context.Context, ssid, psk string) error
	StopHotspot(ctx context.Context) error
	// HasSavedWiFi reports whether any Wi-Fi network other than the
	// device's own access point is saved: whether lowering the access point
	// to look for one could find anything at all.
	HasSavedWiFi(ctx context.Context) (bool, error)
}
