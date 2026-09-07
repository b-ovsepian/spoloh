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
// Connected means joined to an upstream network — not merely that the wifi
// device is active. It is specifically false while the device is only
// serving its own hotspot, because that is the distinction a caller deciding
// whether to keep the hotspot up actually needs.
//
// There is no address field here on purpose. internal/web.LocalAddresses
// already answers "what IP does this device have" by reading Go's own net
// package; it needs no NetworkManager and keeps working even when nmcli is
// broken, so this type does not duplicate it.
type Status struct {
	Connected bool
	SSID      string
	HotspotUp bool
}

// Manager is the surface the rest of the daemon uses. Everything above it is
// tested against Fake; only NMCLI ever runs a command.
type Manager interface {
	Status(ctx context.Context) (Status, error)
	Scan(ctx context.Context) ([]Network, error)
	Connect(ctx context.Context, ssid, psk string) error
	StartHotspot(ctx context.Context, ssid, psk string) error
	StopHotspot(ctx context.Context) error
}
