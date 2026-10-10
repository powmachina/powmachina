package p2p

import "net"

// Netgroup is the group addresses are distributed into addrman buckets by.
//
// Without it an attacker only has to buy thousands of addresses in one range and fill the whole
// table with them — that is exactly an eclipse attack. So the decision is not made by the individual
// IP but by the group: /16 for IPv4, /32 for IPv6, and onion addresses have a group of their own.
// When an ASN map is available, the ASN is used, which is far tighter than a /16.
type ASNLookup func(net.IP) (uint32, bool)

// Netgroup returns the group's identifier. The first byte says what kind of group it is, so that
// different kinds cannot coincide.
func Netgroup(ip net.IP, asn ASNLookup) []byte {
	if ip == nil {
		return []byte{0}
	}
	if asn != nil {
		if num, ok := asn(ip); ok {
			return []byte{1, byte(num >> 24), byte(num >> 16), byte(num >> 8), byte(num)}
		}
	}
	if v4 := ip.To4(); v4 != nil {
		return []byte{2, v4[0], v4[1]} // /16
	}
	if v6 := ip.To16(); v6 != nil {
		return append([]byte{3}, v6[:4]...) // /32
	}
	return []byte{0}
}

// OnionGroup is the group for .onion addresses — they have no IP and must not be mixed with IPv6.
func OnionGroup(addr string) []byte {
	if len(addr) == 0 {
		return []byte{0}
	}
	// Onion addresses have no relation to the network topology, so they form one group.
	// That limits how many buckets somebody with thousands of onion addresses can occupy.
	return []byte{4}
}
