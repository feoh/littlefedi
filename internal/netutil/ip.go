// Package netutil contains shared network-safety helpers.
package netutil

import "net"

// IsPublicIP reports whether ip is suitable for an outbound request to an
// untrusted remote service. It rejects private and special-use
// ranges, even when net.IP.IsGlobalUnicast classifies them as unicast.
func IsPublicIP(ip net.IP) bool {
	if ip == nil || !ip.IsGlobalUnicast() || ip.IsLoopback() || ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() {
		return false
	}

	if ip4 := ip.To4(); ip4 != nil {
		switch {
		case ip4[0] == 0:
			return false // "this network" (0.0.0.0/8)
		case ip4[0] == 100 && ip4[1] >= 64 && ip4[1] <= 127:
			return false // shared address space (100.64.0.0/10)
		case ip4[0] == 192 && ip4[1] == 0 && ip4[2] == 0:
			return false // IETF protocol assignments (192.0.0.0/24)
		case ip4[0] == 192 && ip4[1] == 0 && ip4[2] == 2:
			return false // TEST-NET-1
		case ip4[0] == 192 && ip4[1] == 88 && ip4[2] == 99:
			return false // deprecated 6to4 relay anycast
		case ip4[0] == 198 && (ip4[1] == 18 || ip4[1] == 19):
			return false // benchmarking (198.18.0.0/15)
		case ip4[0] == 198 && ip4[1] == 51 && ip4[2] == 100:
			return false // TEST-NET-2
		case ip4[0] == 203 && ip4[1] == 0 && ip4[2] == 113:
			return false // TEST-NET-3
		case ip4[0] >= 240:
			return false // reserved and limited broadcast (240.0.0.0/4)
		}
		return true
	}

	// Documentation and IPv4-translation prefixes are not valid federation or
	// push destinations. Blocking translation prefixes also prevents encoding a
	// private IPv4 destination inside an otherwise unicast IPv6 address.
	if inIPv6Prefix(ip, net.ParseIP("64:ff9b::"), 96) ||
		inIPv6Prefix(ip, net.ParseIP("64:ff9b:1::"), 48) ||
		inIPv6Prefix(ip, net.ParseIP("100::"), 64) ||
		inIPv6Prefix(ip, net.ParseIP("2001:db8::"), 32) ||
		inIPv6Prefix(ip, net.ParseIP("3fff::"), 20) ||
		inIPv6Prefix(ip, net.ParseIP("5f00::"), 16) {
		return false
	}

	return true
}

func inIPv6Prefix(ip, network net.IP, bits int) bool {
	return (&net.IPNet{IP: network, Mask: net.CIDRMask(bits, net.IPv6len*8)}).Contains(ip)
}
