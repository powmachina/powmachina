package p2p

import (
	"context"
	"crypto/sha256"
	"errors"
	"net"
	"time"
)

// Bootstrap: how a new node learns about the network.
//
// It is the most sensitive moment of the whole run — the node has nothing to compare against, so
// whoever supplies it with addresses at that moment decides who it will see. Hence three rules:
//
//  1. **At most MaxAddrsPerSeed addresses are taken from one seed.** A seed returning thousands
//     does not get more influence than the others.
//  2. **Several seeds are asked at once** and the addresses are mixed, so that one of them does not
//     decide about the entry.
//  3. **Once the node has addresses, the seeds are not asked at all** — after a restart the addrman
//     is a better source than any seed.
//
// On top of that comes the operational rule from SECURITY.md §5: the seeds have to be on different
// domains and with different people. The Monero scheme, where every seed lives on one domain, fell
// all at once.

const (
	// MaxAddrsPerSeed is the cap on addresses from one seed.
	MaxAddrsPerSeed = 64
	// SeedQueryTimeout is the time allowed for one seed's answer.
	SeedQueryTimeout = 10 * time.Second
	// EnoughAddresses is the number of addresses at which the seeds are no longer asked.
	EnoughAddresses = 200
)

var ErrNoSeeds = errors.New("p2p: no addresses could be obtained from any seed")

// Resolver translates a name into addresses. It is an interface so the bootstrap can be tested
// without a network and so an operator can supply their own DNS.
type Resolver func(ctx context.Context, host string) ([]net.IP, error)

// SystemResolver uses the system DNS.
func SystemResolver(ctx context.Context, host string) ([]net.IP, error) {
	var r net.Resolver
	addrs, err := r.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	out := make([]net.IP, 0, len(addrs))
	for _, a := range addrs {
		out = append(out, a.IP)
	}
	return out, nil
}

// seedSource builds the source address for a given seed.
//
// The source determines which buckets the addresses land in. If every seed had the same source,
// they would occupy the same buckets; this way every seed has a group of its own and one seed can
// influence only a small part of the table.
func seedSource(seed string) net.IP {
	h := sha256.Sum256([]byte("PowMachina/dns-seed/" + seed))
	return net.IPv4(h[0], h[1], h[2], h[3])
}

// Bootstrap fills the addrman with addresses from the seeds. It returns the number of addresses added.
//
// `fixed` are the addresses baked into the binary — they are used when DNS fails or is censored.
func Bootstrap(ctx context.Context, addrs *AddrMan, seeds, fixed []string,
	resolve Resolver, defaultPort uint16) (int, error) {

	if newCount, tried := addrs.Size(); newCount+tried >= EnoughAddresses {
		return 0, nil // the addrman is enough, no need to bother the seeds
	}

	type result struct {
		seed string
		ips  []net.IP
	}
	results := make(chan result, len(seeds))
	for _, seed := range seeds {
		go func(seed string) {
			c, cancel := context.WithTimeout(ctx, SeedQueryTimeout)
			defer cancel()
			ips, err := resolve(c, seed)
			if err != nil {
				results <- result{seed: seed}
				return
			}
			results <- result{seed: seed, ips: ips}
		}(seed)
	}

	added := 0
	for range seeds {
		r := <-results
		source := seedSource(r.seed)
		count := 0
		for _, ip := range r.ips {
			if count >= MaxAddrsPerSeed {
				break // one seed must not flood the table
			}
			if !usableIP(ip, addrs.AllowPrivate) {
				continue
			}
			if addrs.Add(Address{IP: ip, Port: defaultPort, LastSeen: time.Now()}, source) {
				added++
			}
			count++
		}
	}

	if added > 0 {
		return added, nil
	}

	// DNS failed or is censored — we try the addresses baked into the binary.
	for _, hostport := range fixed {
		host, port, err := splitHostPort(hostport, defaultPort)
		if err != nil {
			continue
		}
		ip := net.ParseIP(host)
		if ip == nil || !usableIP(ip, addrs.AllowPrivate) {
			continue
		}
		if addrs.Add(Address{IP: ip, Port: port, LastSeen: time.Now()}, seedSource("fixed")) {
			added++
		}
	}
	if added == 0 {
		return 0, ErrNoSeeds
	}
	return added, nil
}

// usableIP filters out addresses there is no point connecting to. The address table decides, not
// the seed: when the node has private ranges allowed, they pass here too.
func usableIP(ip net.IP, allowPrivate bool) bool {
	return Dialable(Address{IP: ip, Port: 1}, allowPrivate)
}

func splitHostPort(hostport string, defaultPort uint16) (string, uint16, error) {
	host, portStr, err := net.SplitHostPort(hostport)
	if err != nil {
		return hostport, defaultPort, nil // no port — the default is used
	}
	var port int
	for _, c := range portStr {
		if c < '0' || c > '9' {
			return "", 0, errors.New("p2p: an invalid port")
		}
		port = port*10 + int(c-'0')
		if port > 65535 {
			return "", 0, errors.New("p2p: the port is out of range")
		}
	}
	if port == 0 {
		return host, defaultPort, nil
	}
	return host, uint16(port), nil
}
