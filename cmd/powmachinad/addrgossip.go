package main

import (
	"log"
	"math/rand/v2"
	"net"
	"time"

	"powmachina/internal/p2p"
)

// Address gossip (p2p/wire_addr.go).
//
// A node asks every full outbound peer once for the addresses it knows, and tells it where it
// listens itself. Fresh addresses that come in small messages — announcements, not answers to a
// getaddr — go on to two peers, so a new node is known across the network within minutes.
//
// Block-relay connections take no part: that they exchange no addresses is what makes them
// worthless to an attacker who poisons the address tables (SECURITY.md §4).

const (
	// fullDagPeers is how many outbound peers holding the full DAG a node that holds it keeps,
	// replacing a light peer when it knows of one. Shares only travel between full-DAG nodes, and
	// two are enough not to depend on one of them.
	fullDagPeers = 2
	// addrRelayMax: only a message this small is an announcement worth passing on.
	addrRelayMax = 10
	// addrRelayFresh: an address older than this is history, not news.
	addrRelayFresh = 10 * time.Minute
	// addrRelayFanout is the number of peers an announcement goes on to.
	addrRelayFanout = 2
)

// addrPeer says whether a peer takes part in address gossip.
func addrPeer(p *p2p.Peer) bool {
	return p.Kind == p2p.PeerOutboundFull || p.Kind == p2p.PeerInbound
}

// greetAddrs is sent to a full outbound peer right after connecting: a getaddr, and where we
// listen, if we do.
func (n *Node) greetAddrs(conn *p2p.Conn) {
	_ = conn.Send(&p2p.Message{Command: p2p.CmdGetAddr})
	port := uint16(n.listenPort.Load())
	if port == 0 {
		return
	}
	self := p2p.Address{IP: net.IPv4zero, Port: port, Services: n.services(), LastSeen: time.Now()}
	if payload, err := p2p.EncodeAddr([]p2p.Address{self}); err == nil {
		_ = conn.Send(&p2p.Message{Command: p2p.CmdAddr, Payload: payload})
	}
}

// answerGetAddr answers an inbound peer's first getaddr. Outbound peers are not answered: an
// attacker who accepts our connection could otherwise map which addresses we know.
func (n *Node) answerGetAddr(conn *p2p.Conn, peer *p2p.Peer) {
	if peer.Kind != p2p.PeerInbound || !peer.FirstGetAddr() {
		return
	}
	payload, err := p2p.EncodeAddr(n.addrs.Sample(p2p.MaxAddrPerMessage))
	if err != nil {
		return
	}
	_ = conn.Send(&p2p.Message{Command: p2p.CmdAddr, Payload: payload})
}

// acceptAddrs files the addresses a peer sent, as many as its rate limit allows.
func (n *Node) acceptAddrs(peer *p2p.Peer, list []p2p.Address) {
	if !addrPeer(peer) {
		return
	}
	now := time.Now()
	list = list[:peer.AllowAddrs(len(list), now)]
	var news []p2p.Address
	for _, a := range list {
		if a.IP.IsUnspecified() {
			// "Where you see me from": only the side that was dialled sees it, and only for the
			// peer itself — anything else naming nobody is dropped.
			if peer.Kind != p2p.PeerInbound || len(list) != 1 {
				continue
			}
			a.IP, a.LastSeen = peer.Addr.IP, now
		}
		if a.LastSeen.After(now.Add(addrRelayFresh)) {
			a.LastSeen = now // a clock from the future earns no precedence
		}
		if n.addrs.Add(a, peer.Addr.IP) && len(list) <= addrRelayMax && now.Sub(a.LastSeen) < addrRelayFresh {
			news = append(news, a)
		}
	}
	if len(news) > 0 {
		n.relayAddrs(news, addrOf(peer))
	}
}

// relayAddrs passes announcements on to a few gossiping peers, never back to their source.
func (n *Node) relayAddrs(list []p2p.Address, except string) {
	payload, err := p2p.EncodeAddr(list)
	if err != nil {
		return
	}
	n.mu.Lock()
	targets := make([]*p2p.Conn, 0, len(n.conns))
	for key, c := range n.conns {
		if key != except && n.gossips[key] {
			targets = append(targets, c)
		}
	}
	n.mu.Unlock()
	rand.Shuffle(len(targets), func(i, j int) { targets[i], targets[j] = targets[j], targets[i] })
	if len(targets) > addrRelayFanout {
		targets = targets[:addrRelayFanout]
	}
	for _, c := range targets {
		_ = c.Send(&p2p.Message{Command: p2p.CmdAddr, Payload: payload})
	}
}

// swapForFullDag gives up the youngest light outbound peer when a node holding the full DAG has
// fewer than fullDagPeers peers like it and knows the address of one more. The freed slot is
// filled with that address straight away; the outbound loop would fill it with any.
func (n *Node) swapForFullDag() {
	if !n.epochs.isFull() || n.peers.CountServices(p2p.PeerOutboundFull, p2p.ServiceFullDag) >= fullDagPeers {
		return
	}
	if _, err := n.peers.SelectOutboundServices(p2p.PeerOutboundFull, p2p.ServiceFullDag); err != nil {
		return // nobody to swap to
	}
	light := n.peers.NewestWithout(p2p.PeerOutboundFull, p2p.ServiceFullDag)
	if light == nil {
		return
	}
	n.mu.Lock()
	c := n.conns[addrOf(light)]
	n.mu.Unlock()
	if c == nil {
		return
	}
	log.Printf("hanging up on light peer %v for one holding the full DAG", light.Addr.IP)
	c.Close()
	// The read loop cleans up asynchronously; the slot is free once the manager forgets the peer.
	n.peers.Remove(light.Addr)
	_ = n.FillOutboundOnce(p2p.PeerOutboundFull)
}
