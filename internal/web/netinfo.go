package web

import (
	"net"
	"os"
	"strings"
)

// LocalAddresses lists the IPv4 addresses an operator could type into a browser
// to reach this device. Loopback is excluded because it is never the answer to
// "what address do I open?", and IPv6 is excluded because this number gets read
// aloud over a phone.
func LocalAddresses() []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []string
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			if v4 := ipNet.IP.To4(); v4 != nil && !v4.IsLoopback() {
				out = append(out, v4.String())
			}
		}
	}
	return out
}

// avahiSocket is where avahi-daemon listens. Its presence is the cheapest
// honest answer to "is anything publishing this device's name" — a file
// check rather than an exec of systemctl, on a device that does this on
// every page load.
const avahiSocket = "/run/avahi-daemon/socket"

// MDNSName returns the name an operator can type instead of an IP address, or
// "" when nothing is publishing one.
//
// The name comes from the system hostname rather than config.Device.Name,
// because avahi publishes the hostname and not our configuration. The daemon
// runs unprivileged and cannot call hostnamectl, so the two can diverge; when
// they do, this line has to show the one that actually resolves. An address
// that does not answer is worse than no address, because someone reads it
// aloud over a phone and then believes the device is broken.
func MDNSName() string { return MDNSNameFrom(avahiSocket, os.Hostname) }

// MDNSNameFrom is MDNSName with its two dependencies injected, so a test can
// exercise it without an avahi installation or a particular hostname.
func MDNSNameFrom(socket string, hostname func() (string, error)) string {
	if _, err := os.Stat(socket); err != nil {
		return ""
	}
	h, err := hostname()
	if err != nil {
		return ""
	}
	// avahi publishes the first label only.
	label, _, _ := strings.Cut(h, ".")
	if label == "" {
		return ""
	}
	return label + ".local"
}
