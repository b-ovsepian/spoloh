// Package timedate wraps systemd's time and date settings. It is the write
// side of the tool internal/clock already reads: clock.Timedatectl asks
// `timedatectl show -p NTPSynchronized` whether this board knows what time
// it is, and this package is what lets an operator fix the answer when it is
// no.
//
// It shells out to timedatectl rather than speaking D-Bus, for the same
// reason netmgr shells out to nmcli and player to aplay: the project builds
// with CGO_ENABLED=0 so one laptop can cross-compile for every board, and it
// takes no dependency the module graph does not already have.
package timedate

import "context"

// State is what the device's clock configuration looks like right now — the
// parseable half of `timedatectl show`, which is preferred over
// `timedatectl status` because status is localised prose meant for a person
// and this is a machine reading it.
type State struct {
	// Timezone is the zone the *system* is set to, which is not necessarily
	// config.Device.Timezone: those two drifting apart is the failure the
	// /time screen exists to make visible and to fix.
	Timezone string
	// LocalRTC is systemd's "the hardware clock keeps local time" flag. The
	// dashboard only reports it; nothing here sets it. A board with no RTC
	// at all — the common case for a Pi — reports no.
	LocalRTC bool
	// CanNTP is whether the host has an NTP client systemd can drive at all.
	// Without one, offering the operator an NTP switch would be offering a
	// switch wired to nothing.
	CanNTP bool
	// NTP is whether network time synchronisation is enabled. It is also
	// what decides whether the clock may be set by hand: systemd refuses
	// set-time while NTP is on, so the screen must say so rather than
	// letting the operator submit something that will be rejected.
	NTP bool
	// NTPSynchronized is whether the clock has actually been synchronised —
	// the same property internal/clock gates the scheduler on. NTP true with
	// this false is a board that is trying and has not succeeded, which is
	// precisely the state an operator needs to see rather than guess at.
	NTPSynchronized bool
}

// Manager is the surface the rest of the daemon uses. Everything above it is
// tested against Fake; only Timedatectl ever runs a command.
type Manager interface {
	// Show reports the current clock configuration.
	Show(ctx context.Context) (State, error)
	// Zones lists the time zones this host knows, for the picker on the
	// /time screen. Read-only and not polkit-gated, unlike everything below
	// it.
	Zones(ctx context.Context) ([]string, error)
	// SetTimezone changes the system time zone. The caller is responsible
	// for keeping config.Device.Timezone in step; see web.handleTimeZone
	// for the order that makes a half-applied change impossible.
	SetTimezone(ctx context.Context, tz string) error
	// SetNTP turns network time synchronisation on or off.
	SetNTP(ctx context.Context, on bool) error
	// SetTime sets the clock by hand. layout is "2006-01-02 15:04:05", the
	// form timedatectl accepts. systemd refuses this while NTP is on.
	SetTime(ctx context.Context, stamp string) error
	// ApplyNTPServer records the NTP server the operator chose and asks the
	// system to adopt it. timedatectl cannot set this: it lives in
	// /etc/systemd/timesyncd.conf.d/, which the unprivileged spoloh user
	// cannot write. See Timedatectl.ApplyNTPServer for the two-part
	// mechanism that gets it there.
	ApplyNTPServer(ctx context.Context, server string) error
}
