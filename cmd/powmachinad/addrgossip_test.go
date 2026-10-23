package main

import (
	"net"
	"testing"
	"time"

	"powmachina/internal/chain"
	"powmachina/internal/mh"
	"powmachina/internal/p2p"
)

func knows(n *Node, a p2p.Address, services uint64) bool {
	got, err := n.addrs.SelectServices(services, func(x p2p.Address) bool { return x.String() != a.String() })
	return err == nil && got.String() == a.String()
}

// A node that dials out says where it listens, and the one it dialled hands that on to whoever
// asks: a third node learns the address, with the services, without ever having been told it.
func TestNodeAnnouncesItselfAndGossipsAddresses(t *testing.T) {
	hub, hubAddr, stopHub := listeningNode(t)
	defer stopHub()
	full, fullAddr, stopFull := listeningNode(t)
	defer stopFull()

	full.addrs.Add(hubAddr, net.ParseIP("88.198.113.1"))
	if err := full.FillOutboundOnce(p2p.PeerOutboundFull); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 2*time.Second, func() bool {
		return knows(hub, fullAddr, p2p.ServiceFullDag)
	}, "the hub did not learn where the dialler listens")

	newcomer := NewNode(chain.RegTest, chain.RegtestGenesis(), mh.TestProfile)
	defer newcomer.Close()
	newcomer.addrs.Add(hubAddr, net.ParseIP("88.198.113.1"))
	if err := newcomer.FillOutboundOnce(p2p.PeerOutboundFull); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 2*time.Second, func() bool {
		return knows(newcomer, fullAddr, p2p.ServiceFullDag)
	}, "the getaddr answer did not bring the full node's address")
}

// A node listening on a private address on a public network does not announce itself: its peers
// would file it under the public address they see it from.
func TestPrivateListenerIsNotAnnounced(t *testing.T) {
	for _, c := range []struct {
		params   chain.NetParams
		genesis  *chain.Header
		announce bool
	}{
		{chain.TestNet, chain.TestnetGenesis(), false},
		{chain.RegTest, chain.RegtestGenesis(), true}, // private throughout
	} {
		node := NewNode(c.params, c.genesis, mh.TestProfile)
		ln, err := p2p.Listen("127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		ln.Close()
		node.Listen(ln) // takes the address, then returns on the closed listener
		node.Close()
		if got := node.listenPort.Load() != 0; got != c.announce {
			t.Fatalf("%s: a node listening on 127.0.0.1 announces itself: %v", c.params.Name, got)
		}
	}
}

// Addresses from a block-relay peer are ignored, and a self-announcement only counts from a peer
// that dialled us.
func TestAddrsFromTheWrongPeersAreDropped(t *testing.T) {
	node := NewNode(chain.RegTest, chain.RegtestGenesis(), mh.TestProfile)
	defer node.Close()
	other := p2p.Address{IP: net.ParseIP("88.198.113.7"), Port: 27696, LastSeen: time.Now()}
	self := p2p.Address{IP: net.IPv4zero, Port: 27696, LastSeen: time.Now()}
	from := p2p.Address{IP: net.ParseIP("45.83.104.1"), Port: 27696}

	node.acceptAddrs(p2p.NewPeer(from, p2p.PeerOutboundBlock), []p2p.Address{other})
	node.acceptAddrs(p2p.NewPeer(from, p2p.PeerOutboundFull), []p2p.Address{self})
	if n, _ := node.addrs.Size(); n != 0 {
		t.Fatalf("%d addresses filed from peers that do not gossip or announce", n)
	}
	node.acceptAddrs(p2p.NewPeer(from, p2p.PeerInbound), []p2p.Address{self})
	if !knows(node, p2p.Address{IP: from.IP, Port: 27696}, 0) {
		t.Fatal("an inbound peer's self-announcement was not filed under its address")
	}
}

// A node holding the full DAG with only light peers hangs up on one of them for a full-DAG node it
// knows of — shares would otherwise never reach it.
func TestFullDagNodeSwapsALightPeer(t *testing.T) {
	light, lightAddr, stopLight := listeningNode(t)
	defer stopLight()
	light.epochs.full.Store(false)
	_, fullAddr, stopFull := listeningNode(t)
	defer stopFull()

	client := NewNode(chain.RegTest, chain.RegtestGenesis(), mh.TestProfile)
	defer client.Close()
	client.addrs.Add(lightAddr, net.ParseIP("88.198.113.1"))
	if err := client.FillOutboundOnce(p2p.PeerOutboundFull); err != nil {
		t.Fatal(err)
	}
	if got := client.peers.CountServices(p2p.PeerOutboundFull, p2p.ServiceFullDag); got != 0 {
		t.Fatalf("setup: %d full-DAG peers", got)
	}
	claimed := fullAddr
	claimed.Services = p2p.ServiceFullNode | p2p.ServiceFullDag
	client.addrs.Add(claimed, net.ParseIP("45.83.104.1"))

	client.swapForFullDag()
	if got := client.peers.CountServices(p2p.PeerOutboundFull, p2p.ServiceFullDag); got != 1 {
		t.Fatalf("after the swap %d full-DAG peers, expected 1", got)
	}
	waitFor(t, 2*time.Second, func() bool {
		client.mu.Lock()
		defer client.mu.Unlock()
		_, still := client.conns[lightAddr.String()]
		return !still && len(client.conns) == 1
	}, "the light peer is still connected")

	// With enough full-DAG peers it swaps no more.
	client.addrs.Add(lightAddr, net.ParseIP("88.198.113.1"))
	client.swapForFullDag()
	if got := client.peers.Count(p2p.PeerOutboundFull); got != 1 {
		t.Fatalf("a light node has no reason to swap, yet %d outbound peers", got)
	}
}
