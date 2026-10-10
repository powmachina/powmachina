package p2p

import "net"

// Address routability — what the node may dial.
//
// The reason is operational, not theoretical: a node trying to connect from a public server to
// private ranges looks to the provider like **scanning an internal network**. Hosting providers
// send abuse reports and lock servers for it — and one poisoned DNS seed or one address from gossip
// pointing
// at 10.0.0.0/8 is enough.
//
// So the rule is simple: **on the public network, only what is routable on the internet is dialled.**
// A private deployment (sentry nodes on an RFC 1918 range, a testnet on the LAN) is turned on
// explicitly with `-allow-private`; regtest has it on always.

// privateV4 are the ranges nobody routes on the internet. RFC1918 is covered by `IsPrivate`, the
// rest is not — and it is precisely the others (CGNAT, benchmark, documentation) that can do damage,
// because at a provider they often belong to its own infrastructure.
// The list is the same one Bitcoin Core has in `CNetAddr::IsRoutable()` (netaddress.cpp): RFC1918,
// RFC2544, RFC3927, RFC4862, RFC6598, RFC5737, RFC4193 and RFC4843. We stick to it deliberately —
// it is proven by fifteen years of operation and we have no reason to be original.
var privateV4 = []net.IPNet{
	{IP: net.IPv4(0, 0, 0, 0), Mask: net.CIDRMask(8, 32)},       // "this network"
	{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)},   // CGNAT (RFC 6598)
	{IP: net.IPv4(192, 0, 0, 0), Mask: net.CIDRMask(24, 32)},    // IETF protocol assignments
	{IP: net.IPv4(192, 0, 2, 0), Mask: net.CIDRMask(24, 32)},    // TEST-NET-1 (RFC 5737)
	{IP: net.IPv4(198, 18, 0, 0), Mask: net.CIDRMask(15, 32)},   // benchmarking (RFC 2544)
	{IP: net.IPv4(198, 51, 100, 0), Mask: net.CIDRMask(24, 32)}, // TEST-NET-2 (RFC 5737)
	{IP: net.IPv4(203, 0, 113, 0), Mask: net.CIDRMask(24, 32)},  // TEST-NET-3 (RFC 5737)
	{IP: net.IPv4(240, 0, 0, 0), Mask: net.CIDRMask(4, 32)},     // reserved + broadcast
}

// IPv6 ranges that are inside 2000::/3 but are not meant to be routed.
var blockedV6 = []net.IPNet{
	{IP: net.ParseIP("2001:10::"), Mask: net.CIDRMask(28, 128)},  // ORCHID (RFC 4843)
	{IP: net.ParseIP("2001:db8::"), Mask: net.CIDRMask(32, 128)}, // documentation (RFC 3849)
}

// Routable says whether a node on the public network may connect to an address.
//
// It does not cover whether the address is reachable — only whether it makes sense and whether a
// connection attempt would touch somebody else's internal network.
func Routable(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if ip.IsUnspecified() || ip.IsLoopback() || ip.IsMulticast() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() {
		return false
	}
	if ip.IsPrivate() { // RFC1918 and IPv6 ULA (fc00::/7)
		return false
	}
	if v4 := ip.To4(); v4 != nil {
		for i := range privateV4 {
			if privateV4[i].Contains(v4) {
				return false
			}
		}
		return true
	}
	// IPv6: 2000::/3 is the only range that is globally routable today.
	if len(ip) != net.IPv6len || ip[0]&0xe0 != 0x20 {
		return false
	}
	for i := range blockedV6 {
		if blockedV6[i].Contains(ip) {
			return false
		}
	}
	return true
}

// Dialable combines the rule with the network: onion addresses always go (they are routed through a
// proxy, not by IP), and with `allowPrivate` nothing is filtered — that is the mode for a closed
// deployment of your own.
func Dialable(a Address, allowPrivate bool) bool {
	if a.Onion != "" {
		return true
	}
	if a.IP == nil || a.Port == 0 {
		return false
	}
	if allowPrivate {
		return !a.IP.IsUnspecified() && !a.IP.IsMulticast()
	}
	return Routable(a.IP)
}
