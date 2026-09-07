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
type Status struct {
	Connected bool
	SSID      string
	IPv4      string
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
