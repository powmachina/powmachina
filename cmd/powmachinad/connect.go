package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"powmachina/internal/p2p"
)

// Keeping connections up.
//
// Several loops, each solving a different thing:
//
//	outbound — fills the missing outbound connections, always into a different network group
//	feeler   — tries one unverified address every two minutes and hangs up
//	anchors  — after a restart, connects first to the peers that proved themselves
//	ping     — keeps idle connections alive and hangs up on those that do not answer
//	bodies   — pulls block bodies according to the header manifest (bodysync.go)
//
// The feeler is a defence against an eclipse attack: an attacker floods the addrman with
// addresses that lead nowhere, and the normal selection would offer them forever. The feeler
// tries them, moves the working ones to the verified table and sorts the rest, so the table
// stays usable even after a restart (SECURITY.md §4).

// connectTo opens a connection, finishes the handshake and files the peer into a slot.
func (n *Node) connectTo(addr p2p.Address, kind p2p.PeerKind) (*p2p.Conn, *p2p.Peer, error) {
	hostport := fmt.Sprintf("%s:%d", addr.IP, addr.Port)
	conn, err := p2p.Dial(hostport, n.params.Magic)
	if err != nil {
		n.addrs.Attempt(addr)
		return nil, nil, err
	}
	remote, err := conn.Handshake(n.version(), n.nonce, true)
	if err != nil {
		conn.Close()
		n.markSelfOrAttempt(addr, err)
		return nil, nil, err
	}
	peer := p2p.NewPeer(addr, kind)
	if err := peer.CompleteHandshake(remote, n.nonce); err != nil {
		conn.Close()
		n.markSelfOrAttempt(addr, err)
		return nil, nil, err
	}
	n.notePeerClock(remote)
	if _, err := n.peers.Add(peer); err != nil {
		conn.Close()
		return nil, nil, err
	}
	// The connection went all the way through — the address is verified.
	n.addrs.Good(addr)
	return conn, peer, nil
}

// markSelfOrAttempt records a failed attempt — and when the node connected to itself, it
// remembers that the address is its own.
//
// A node behind NAT does not know its public address, so this is the only way to find out (the
// nonce in the handshake). Without it a node that is a DNS seed itself keeps calling itself.
func (n *Node) markSelfOrAttempt(addr p2p.Address, err error) {
	if errors.Is(err, p2p.ErrSelfConnection) {
		if !n.addrs.IsLocal(addr) {
			log.Printf("address %v is our own — dropped from the addrman", addr.IP)
		}
		n.addrs.MarkLocal(addr)
		return
	}
	n.addrs.Attempt(addr)
}

// FeelerOnce tries one unverified address and hangs up right away.
//
// It does not block a slot: a feeler connection does not count towards the slots and is closed
// right after the handshake.
func (n *Node) FeelerOnce() error {
	addr, err := n.addrs.SelectNew()
	if err != nil {
		return err
	}
	if n.peers.IsBanned(addr) {
		return nil
	}
	conn, peer, err := n.connectTo(addr, p2p.PeerFeeler)
	if err != nil {
		return err
	}
	// The address is verified (connectTo called Good) — we do not need the connection any more.
	n.peers.Remove(peer.Addr)
	conn.Close()
	return nil
}

// MaintainOutbound fills the missing outbound connections.
//
// Both kinds of slot are maintained **independently**. If block-relay slots were only filled
// once the full ones were, they would never come about on a small network — and those are
// exactly where the anchor peers that protect the node across a restart come from.
func (n *Node) MaintainOutbound() {
	cfg := p2p.DefaultManagerConfig()
	if n.peers.Count(p2p.PeerOutboundFull) < cfg.MaxOutboundFull {
		_ = n.FillOutboundOnce(p2p.PeerOutboundFull)
	}
	if n.peers.Count(p2p.PeerOutboundBlock)+n.peers.Count(p2p.PeerAnchor) < cfg.MaxOutboundBlock {
		_ = n.FillOutboundOnce(p2p.PeerOutboundBlock)
	}
}

// FillOutboundOnce fills one missing outbound connection of the given kind.
func (n *Node) FillOutboundOnce(kind p2p.PeerKind) error {
	addr, err := n.peers.SelectOutboundCandidate(kind)
	if err != nil {
		return err
	}
	conn, peer, err := n.connectTo(addr, kind)
	if err != nil {
		return err
	}
	n.addConn(addr, conn)
	if !n.addLive(conn) {
		n.removeConn(addr)
		n.peers.Remove(addr)
		conn.Close()
		return errNodeClosing
	}
	log.Printf("outbound connection to %v (%s)", addr.IP, kind)

	// We ask for headers right away — otherwise the node would hang connected and learn nothing.
	if err := n.RequestHeaders(conn); err != nil {
		log.Printf("the headers request failed: %v", err)
	}

	if !n.goTracked(func() {
		defer func() {
			n.removeConn(addr)
			n.removeLive(conn)
			n.peers.Remove(addr)
			conn.Close()
		}()
		n.readLoop(conn, peer, p2p.OutboundPeerLimits())
	}) {
		n.removeConn(addr)
		n.peers.Remove(addr)
		conn.Close()
		return errNodeClosing
	}
	return nil
}

// PingPeers sends a ping on every connection and hangs up on those that did not answer the
// previous one.
//
// Without it every connection dies on the read deadline (ReadTimeout, 90 s). With a block
// every 120 s a connection is idle almost always, so this is not the state of an empty network
// but of normal operation: the network falls apart into individual machines and the log only
// shows nodes reconnecting forever. The node could answer a ping from the start, sending one
// it could not.
func (n *Node) PingPeers(now time.Time) {
	n.mu.Lock()
	conns := make([]*p2p.Conn, 0, len(n.conns))
	for _, c := range n.conns {
		conns = append(conns, c)
	}
	n.mu.Unlock()

	for _, c := range conns {
		if c.PingOverdue(now) {
			log.Printf("peer %v did not answer the ping, hanging up", c.RemoteAddress().IP)
			c.Close() // the read loop ends on an error and cleans up both the peer and the connection
			continue
		}
		msg, err := c.PingMessage(now)
		if err != nil {
			c.Close()
			continue
		}
		if msg == nil {
			continue // the previous ping is still being waited for, the deadline runs from it
		}
		// The write has a 30 s deadline and holds the connection lock. If pings were sent one
		// at a time, one stuck peer would delay the healthy ones past ReadTimeout and the other
		// side would disconnect them.
		go func(c *p2p.Conn) {
			if err := c.Send(msg); err != nil {
				c.ForgetPing()
				c.Close()
			}
		}(c)
	}
}

// StartNetwork starts the maintenance loops. They run until `stop` is closed.
func (n *Node) StartNetwork(stop <-chan struct{}) {
	n.startLoop(stop, p2p.OutboundInterval, n.MaintainOutbound)
	n.startLoop(stop, p2p.FeelerInterval, func() { _ = n.FeelerOnce() })
	n.startLoop(stop, p2p.PingInterval, func() { n.PingPeers(time.Now()) })
	n.startLoop(stop, bodySyncInterval, func() { n.SyncBodiesOnce(time.Now()) })
	n.startLoop(stop, poolExpireInterval, func() { n.expirePools(time.Now()) })
	go n.writeBanListLoop(stop)
}

// startLoop starts a maintenance loop and registers it so `Close` can wait for it.
func (n *Node) startLoop(stop <-chan struct{}, every time.Duration, fn func()) {
	n.goTracked(func() { n.loop(stop, every, fn) })
}

// loop runs `fn` every `every` until `stop` is closed **or until the node is closing**.
//
// The latter matters more than it seems: `Close()` closes storage, and if a driver tick is
// running at that moment, Pebble panics on "pebble: closed". That is why the node has a
// channel of its own which Close closes, and Close waits for the loops to finish.
func (n *Node) loop(stop <-chan struct{}, every time.Duration, fn func()) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-n.closing:
			return
		case <-t.C:
			fn()
		}
	}
}

// Anchor peers: which connections the node makes first after a restart.
//
// It is a defence against an eclipse attack at the most vulnerable moment. Whoever floods the
// addrman with their own addresses waits for a restart — and if the node started from scratch,
// they would take every slot. With anchors the node first returns to the peers that already
// proved themselves.

// SaveAnchors stores the current block-relay peers.
func (n *Node) SaveAnchors(path string) error {
	if path == "" {
		return nil
	}
	anchors := n.peers.Anchors(2)
	var sb strings.Builder
	sb.WriteString("# PowMachina anchor peers — the node connects to these first after a restart\n")
	for _, a := range anchors {
		sb.WriteString(fmt.Sprintf("%s:%d\n", a.IP, a.Port))
	}
	return os.WriteFile(path, []byte(sb.String()), 0o600)
}

// ConnectAnchors connects to the stored anchor peers. A missing file is not an error — on the
// first run there are none yet.
func (n *Node) ConnectAnchors(path string) {
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		host, portStr, err := net.SplitHostPort(line)
		if err != nil {
			continue
		}
		port, err := strconv.ParseUint(portStr, 10, 16)
		if err != nil {
			continue
		}
		addr := p2p.Address{IP: net.ParseIP(host), Port: uint16(port), LastSeen: time.Now()}
		if addr.IP == nil {
			continue
		}
		conn, peer, err := n.connectTo(addr, p2p.PeerAnchor)
		if err != nil {
			log.Printf("anchor %s unreachable: %v", line, err)
			continue
		}
		n.addConn(addr, conn)
		if !n.addLive(conn) {
			n.removeConn(addr)
			n.peers.Remove(addr)
			conn.Close()
			return
		}
		log.Printf("connection to anchor peer %s restored", line)
		if err := n.RequestHeaders(conn); err != nil {
			log.Printf("the headers request failed: %v", err)
		}
		c, pr, a := conn, peer, addr
		if !n.goTracked(func() {
			defer func() {
				n.removeConn(a)
				n.removeLive(c)
				n.peers.Remove(a)
				c.Close()
			}()
			for {
				msg, err := c.Recv()
				if err != nil {
					return
				}
				if err := n.handle(c, pr, msg); err != nil {
					return
				}
			}
		}) {
			n.removeConn(a)
			n.peers.Remove(a)
			c.Close()
			return
		}
	}
}

// Bootstrap fills the addrman: the manually given addresses first, then the network's DNS seeds.
//
// The manual ones take precedence on purpose — whoever entered them knows who they trust, and
// there is no point pushing them towards the seeds. The seeds are only asked when the node has
// nowhere else to start from.
func (n *Node) Bootstrap(ctx context.Context, manual []string) {
	for _, hostport := range manual {
		host, portStr, err := net.SplitHostPort(strings.TrimSpace(hostport))
		if err != nil {
			log.Printf("invalid peer address %q", hostport)
			continue
		}
		port, err := strconv.ParseUint(portStr, 10, 16)
		if err != nil {
			log.Printf("invalid port in %q", hostport)
			continue
		}
		ips, err := net.LookupIP(host)
		if err != nil || len(ips) == 0 {
			log.Printf("the address %q could not be resolved: %v", hostport, err)
			continue
		}
		for _, ip := range ips {
			addr := p2p.Address{IP: ip, Port: uint16(port), LastSeen: time.Now()}
			// The source is the address itself — a manually given peer has no business influencing other peers' buckets.
			n.addrs.Add(addr, ip)
		}
		log.Printf("peer %s added", hostport)
	}

	if len(n.params.DNSSeeds) == 0 && len(n.params.FixedSeeds) == 0 {
		return // the network has no seeds (regtest) — the node relies on -connect or -addnode
	}
	added, err := p2p.Bootstrap(ctx, n.addrs, n.params.DNSSeeds, n.params.FixedSeeds,
		p2p.SystemResolver, n.params.P2PPort)
	switch {
	case err != nil:
		log.Printf("no addresses could be obtained from the seeds: %v", err)
	case added > 0:
		log.Printf("%d addresses obtained from the seeds", added)
	}
}
