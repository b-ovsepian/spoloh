package web

import "net"

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
