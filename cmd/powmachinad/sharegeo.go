package main

import (
	"fmt"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"

	"powmachina/internal/geoip"
	"powmachina/internal/p2p"
)

// The SharePool page's map: which countries the shares this node took first came from. A share's
// origin is the address it came in from — the stratum device, or the peer that relayed it here —
// so it is the country of the miner's node or of a node on the way, not of the miner. Only a
// country is kept, summed per minute; no address is stored, and nothing ties a country to a
// payout address.

// geoMinutes is the window the map shows.
const geoMinutes = 30

type shareGeo struct {
	db  *geoip.DB
	now func() time.Time
	// ignore: addresses of the network's own nodes. A share they relay here entered the network
	// somewhere else, where it was already counted, so it is not counted again — otherwise a public
	// node fed by the network's other nodes would put every share in its own country.
	ignore []netip.Prefix

	mu      sync.Mutex
	minute  [geoMinutes]int64 // the unix minute each bucket holds
	buckets [geoMinutes]map[string]int
	unknown [geoMinutes]int
	// seen counts since the start, so a reader can tell new shares from two snapshots.
	seen        map[string]uint64
	seenUnknown uint64
}

func newShareGeo(db *geoip.DB) *shareGeo {
	return &shareGeo{db: db, now: time.Now, seen: map[string]uint64{}}
}

// note files one share that came from origin (host or host:port; empty = this machine).
func (g *shareGeo) note(origin string) {
	if g == nil {
		return
	}
	cc, ok := "", false
	if host := hostOf(origin); host != "" {
		if a, err := netip.ParseAddr(host); err == nil {
			a = a.Unmap()
			for _, p := range g.ignore {
				if p.Contains(a) {
					return
				}
			}
			if p2p.Routable(net.IP(a.AsSlice())) {
				cc, ok = g.db.Country(a)
			}
		}
	}
	m := g.now().Unix() / 60
	g.mu.Lock()
	defer g.mu.Unlock()
	i := int(m % geoMinutes)
	if g.minute[i] != m {
		g.minute[i], g.buckets[i], g.unknown[i] = m, map[string]int{}, 0
	}
	if !ok {
		g.unknown[i]++
		g.seenUnknown++
		return
	}
	g.buckets[i][cc]++
	g.seen[cc]++
}

// snapshot sums the window.
func (g *shareGeo) snapshot() (window map[string]int, seen map[string]uint64, unknown int, seenUnknown uint64) {
	window, seen = map[string]int{}, map[string]uint64{}
	if g == nil {
		return
	}
	m := g.now().Unix() / 60
	g.mu.Lock()
	defer g.mu.Unlock()
	for i := range g.buckets {
		if g.minute[i] <= m-geoMinutes || g.buckets[i] == nil {
			continue
		}
		for cc, n := range g.buckets[i] {
			window[cc] += n
		}
		unknown += g.unknown[i]
	}
	for cc, n := range g.seen {
		seen[cc] = n
	}
	return window, seen, unknown, g.seenUnknown
}

func hostOf(origin string) string {
	if origin == "" {
		return ""
	}
	if h, _, err := net.SplitHostPort(origin); err == nil {
		return h
	}
	return origin
}

// SetShareGeo turns the map on with the database; without it getsharegeo says it is off. Shares
// relayed by an address in ignore are not counted (-geoip-ignore).
func (n *Node) SetShareGeo(db *geoip.DB, ignore []netip.Prefix) {
	n.geo = newShareGeo(db)
	n.geo.ignore = ignore
}

// parsePrefixes reads "1.2.3.4,10.0.0.0/8,2a01::/32" — an address without a length is one host.
func parsePrefixes(s string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, f := range strings.Split(s, ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		if p, err := netip.ParsePrefix(f); err == nil {
			out = append(out, p.Masked())
			continue
		}
		a, err := netip.ParseAddr(f)
		if err != nil {
			return nil, fmt.Errorf("%q is neither an address nor a network", f)
		}
		out = append(out, netip.PrefixFrom(a.Unmap(), a.Unmap().BitLen()))
	}
	return out, nil
}

// ShareGeo implements rpc.Backend.
func (n *Node) ShareGeo() (bool, int, map[string]int, map[string]uint64, int, uint64) {
	if n.geo == nil {
		return false, geoMinutes, nil, nil, 0, 0
	}
	w, s, u, su := n.geo.snapshot()
	return true, geoMinutes, w, s, u, su
}
