package main

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"powmachina/internal/chain"
	"powmachina/internal/mh"
	"powmachina/internal/p2p"
)

// listeningNode starts a node that listens and returns its address.
func listeningNode(t *testing.T) (*Node, p2p.Address, func()) {
	t.Helper()
	node := NewNode(chain.RegTest, chain.RegtestGenesis(), mh.TestProfile)
	ln, err := p2p.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go node.Listen(ln)
	tcp := ln.Addr().(*net.TCPAddr)
	addr := p2p.Address{IP: tcp.IP, Port: uint16(tcp.Port), LastSeen: time.Now()}
	return node, addr, func() { ln.Close(); node.Close() }
}

// The feeler has to try an unverified address and move it among the verified ones — and not keep the connection.
func TestFeelerPromotesWorkingAddress(t *testing.T) {
	_, peerAddr, cleanup := listeningNode(t)
	defer cleanup()

	client := NewNode(chain.RegTest, chain.RegtestGenesis(), mh.TestProfile)
	defer client.Close()
	client.addrs.Add(peerAddr, net.ParseIP("88.198.113.1"))

	if newCount, tried := client.addrs.Size(); newCount != 1 || tried != 0 {
		t.Fatalf("at the start expected 1 unverified and 0 verified, got %d/%d", newCount, tried)
	}

	if err := client.FeelerOnce(); err != nil {
		t.Fatalf("the feeler failed: %v", err)
	}

	newCount, tried := client.addrs.Size()
	if tried != 1 {
		t.Fatalf("the address should have moved among the verified ones, got %d verified", tried)
	}
	if newCount != 0 {
		t.Fatalf("nothing should have stayed among the unverified ones, got %d", newCount)
	}
	// The feeler must hold neither a slot nor a connection.
	if got := client.peers.Count(p2p.PeerFeeler); got != 0 {
		t.Fatalf("the feeler left a connection open (%d)", got)
	}
	client.mu.Lock()
	conns := len(client.conns)
	client.mu.Unlock()
	if conns != 0 {
		t.Fatalf("the feeler kept a connection in the registry (%d)", conns)
	}
}

// A dead address must not make it among the verified ones — that is the whole point of the feeler.
func TestFeelerDoesNotPromoteDeadAddress(t *testing.T) {
	client := NewNode(chain.RegTest, chain.RegtestGenesis(), mh.TestProfile)
	defer client.Close()

	// A port nobody listens on.
	ln, _ := p2p.Listen("127.0.0.1:0")
	dead := ln.Addr().(*net.TCPAddr)
	ln.Close()

	client.addrs.Add(p2p.Address{IP: dead.IP, Port: uint16(dead.Port), LastSeen: time.Now()},
		net.ParseIP("88.198.113.1"))

	if err := client.FeelerOnce(); err == nil {
		t.Fatal("the feeler should have failed on a dead address")
	}
	if _, tried := client.addrs.Size(); tried != 0 {
		t.Fatalf("a dead address made it among the verified ones (%d)", tried)
	}
}

// The node has to fill an outbound connection by itself from the addresses it knows.
func TestOutboundFillConnects(t *testing.T) {
	server, peerAddr, cleanup := listeningNode(t)
	defer cleanup()

	client := NewNode(chain.RegTest, chain.RegtestGenesis(), mh.TestProfile)
	defer client.Close()
	client.addrs.Add(peerAddr, net.ParseIP("88.198.113.1"))

	if err := client.FillOutboundOnce(p2p.PeerOutboundFull); err != nil {
		t.Fatalf("filling the connection failed: %v", err)
	}
	if got := client.peers.Count(p2p.PeerOutboundFull); got != 1 {
		t.Fatalf("expected 1 outbound connection, got %d", got)
	}
	// The other side has to see an inbound connection.
	deadline := time.Now().Add(2 * time.Second)
	for server.peers.Count(p2p.PeerInbound) == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if got := server.peers.Count(p2p.PeerInbound); got != 1 {
		t.Fatalf("the other side does not see the connection (%d)", got)
	}
	// The connection has to be usable for communication.
	client.mu.Lock()
	conns := len(client.conns)
	client.mu.Unlock()
	if conns != 1 {
		t.Fatalf("the connection is not in the registry (%d)", conns)
	}
}

// The loops have to be stoppable.
func TestNetworkLoopsStop(t *testing.T) {
	node := NewNode(chain.RegTest, chain.RegtestGenesis(), mh.TestProfile)
	defer node.Close()
	for i := 0; i < 5; i++ {
		node.addrs.Add(p2p.Address{IP: net.ParseIP(fmt.Sprintf("5.9.%d.1", i)),
			Port: 8333, LastSeen: time.Now()}, net.ParseIP("88.198.113.1"))
	}
	stop := make(chan struct{})
	node.StartNetwork(stop)
	time.Sleep(100 * time.Millisecond)
	close(stop)
	time.Sleep(100 * time.Millisecond) // the loops have to end without a panic
}

// Block-relay slots have to be filled independently of the full ones — otherwise they would
// never come about on a small network and the anchor peers would be missing with them (found
// during a live trial).
func TestBlockRelaySlotFillsIndependently(t *testing.T) {
	_, peerAddr, cleanup := listeningNode(t)
	defer cleanup()

	client := NewNode(chain.RegTest, chain.RegtestGenesis(), mh.TestProfile)
	defer client.Close()
	client.addrs.Add(peerAddr, net.ParseIP("88.198.113.1"))

	// Straight for a block-relay slot, even though the full slots are empty.
	if err := client.FillOutboundOnce(p2p.PeerOutboundBlock); err != nil {
		t.Fatalf("the block-relay connection failed: %v", err)
	}
	if got := client.peers.Count(p2p.PeerOutboundBlock); got != 1 {
		t.Fatalf("expected 1 block-relay connection, got %d", got)
	}
	// An anchor is taken precisely from the block-relay connections.
	if anchors := client.peers.Anchors(2); len(anchors) != 1 {
		t.Fatalf("expected 1 anchor candidate, got %d", len(anchors))
	}
}

// Anchor peers have to be saved and loaded again after a restart.
func TestAnchorsSurviveRestart(t *testing.T) {
	server, peerAddr, cleanup := listeningNode(t)
	defer cleanup()

	first := NewNode(chain.RegTest, chain.RegtestGenesis(), mh.TestProfile)
	first.addrs.Add(peerAddr, net.ParseIP("88.198.113.1"))
	if err := first.FillOutboundOnce(p2p.PeerOutboundBlock); err != nil {
		t.Fatal(err)
	}
	path := t.TempDir() + "/anchors.txt"
	if err := first.SaveAnchors(path); err != nil {
		t.Fatal(err)
	}
	first.Close()

	second := NewNode(chain.RegTest, chain.RegtestGenesis(), mh.TestProfile)
	defer second.Close()
	second.ConnectAnchors(path)

	if got := second.peers.Count(p2p.PeerAnchor); got != 1 {
		t.Fatalf("after the restart expected 1 restored anchor connection, got %d", got)
	}
	deadline := time.Now().Add(2 * time.Second)
	for server.peers.Count(p2p.PeerInbound) < 2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
}

// A manually given peer has to get into the addrman and the maintenance loop has to connect to it.
func TestAddnodeFeedsAddrman(t *testing.T) {
	_, peerAddr, cleanup := listeningNode(t)
	defer cleanup()

	client := NewNode(chain.RegTest, chain.RegtestGenesis(), mh.TestProfile)
	defer client.Close()

	hostport := fmt.Sprintf("%s:%d", peerAddr.IP, peerAddr.Port)
	client.Bootstrap(context.Background(), []string{hostport})

	if newCount, _ := client.addrs.Size(); newCount == 0 {
		t.Fatal("the manually given peer did not get into the addrman")
	}
	if err := client.FillOutboundOnce(p2p.PeerOutboundFull); err != nil {
		t.Fatalf("the connection to the manually given peer failed: %v", err)
	}
	// After a successful connection the address is verified.
	if _, tried := client.addrs.Size(); tried != 1 {
		t.Fatalf("the address should have moved among the verified ones, got %d", tried)
	}
}

// Invalid input must not knock the node over.
func TestBootstrapIgnoresGarbage(t *testing.T) {
	node := NewNode(chain.RegTest, chain.RegtestGenesis(), mh.TestProfile)
	defer node.Close()
	node.Bootstrap(context.Background(), []string{"no-port", "host:99999", ":::", ""})
	if newCount, tried := node.addrs.Size(); newCount != 0 || tried != 0 {
		t.Fatalf("nothing should have been added from nonsense: %d/%d", newCount, tried)
	}
}

// A connection with no traffic has to be kept alive by the ping: the peer answers with a pong
// and the node then waits for nothing. Without it the connection dies on ReadTimeout.
func TestPingKeepsIdleConnectionAlive(t *testing.T) {
	_, peerAddr, cleanup := listeningNode(t)
	defer cleanup()

	client := NewNode(chain.RegTest, chain.RegtestGenesis(), mh.TestProfile)
	defer client.Close()
	client.addrs.Add(peerAddr, net.ParseIP("88.198.113.1"))
	if err := client.FillOutboundOnce(p2p.PeerOutboundFull); err != nil {
		t.Fatalf("filling the connection failed: %v", err)
	}

	now := time.Now()
	client.PingPeers(now)

	// The pong has to arrive and close the wait — we check that at a time well past PingTimeout.
	future := now.Add(2 * p2p.PingTimeout)
	deadline := time.Now().Add(2 * time.Second)
	for {
		client.mu.Lock()
		conn := client.conns[peerAddr.String()]
		client.mu.Unlock()
		if conn == nil {
			t.Fatal("the connection disappeared even though the peer answers the ping")
		}
		if !conn.PingOverdue(future) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the pong did not arrive within two seconds")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// A further round of pings must not hang up a healthy connection.
	client.PingPeers(future)
	client.mu.Lock()
	conns := len(client.conns)
	client.mu.Unlock()
	if conns != 1 {
		t.Fatalf("the healthy connection should have stayed, got %d", conns)
	}
}

// A peer that does not answer a ping has to be hung up after PingTimeout — a connection nobody
// answers on holds a slot and the node learns nothing from it.
func TestPingDropsSilentPeer(t *testing.T) {
	ln, err := p2p.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	tcp := ln.Addr().(*net.TCPAddr)
	peerAddr := p2p.Address{IP: tcp.IP, Port: uint16(tcp.Port), LastSeen: time.Now()}

	// The peer finishes the handshake and then just stays silent.
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		conn := p2p.NewConn(c, chain.RegTest.Magic)
		ver := &p2p.VersionMsg{Protocol: p2p.ProtocolVersion, Nonce: 0xDEAD,
			UserAgent: "/silent-peer/", Relay: true}
		if _, err := conn.Handshake(ver, 0xDEAD, false); err != nil {
			conn.Close()
			return
		}
		for {
			if _, err := conn.Recv(); err != nil {
				conn.Close()
				return
			}
		}
	}()

	client := NewNode(chain.RegTest, chain.RegtestGenesis(), mh.TestProfile)
	defer client.Close()
	client.addrs.Add(peerAddr, net.ParseIP("88.198.113.1"))
	if err := client.FillOutboundOnce(p2p.PeerOutboundFull); err != nil {
		t.Fatalf("filling the connection failed: %v", err)
	}

	now := time.Now()
	client.PingPeers(now)                          // the ping went out, no answer will come
	client.PingPeers(now.Add(p2p.PingTimeout / 2)) // still waiting
	client.mu.Lock()
	conns := len(client.conns)
	client.mu.Unlock()
	if conns != 1 {
		t.Fatalf("the connection was hung up before PingTimeout (%d)", conns)
	}

	client.PingPeers(now.Add(p2p.PingTimeout + time.Second))

	deadline := time.Now().Add(2 * time.Second)
	for {
		client.mu.Lock()
		conns = len(client.conns)
		client.mu.Unlock()
		if conns == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the silent peer was not hung up")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := client.peers.Count(p2p.PeerOutboundFull); got != 0 {
		t.Fatalf("the slot after the hung-up peer was not freed (%d)", got)
	}
}

// The node sends a ping's payload back, so its length has to be fixed — otherwise a peer would
// send a ping with a megabyte of data and the node would pay for the answer.
func TestOversizedPingIsRejected(t *testing.T) {
	node, peerAddr, cleanup := listeningNode(t)
	defer cleanup()

	conn, err := p2p.Dial(fmt.Sprintf("%s:%d", peerAddr.IP, peerAddr.Port), chain.RegTest.Magic)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ver := &p2p.VersionMsg{Protocol: p2p.ProtocolVersion, Timestamp: time.Now().Unix(),
		Nonce: 0xFACE, UserAgent: "/test/"}
	if _, err := conn.Handshake(ver, 0xFACE, true); err != nil {
		t.Fatal(err)
	}

	if err := conn.Send(&p2p.Message{Command: p2p.CmdPing, Payload: make([]byte, 64*1024)}); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Recv(); err == nil {
		t.Fatal("the node answered an oversized ping instead of disconnecting")
	}

	deadline := time.Now().Add(2 * time.Second)
	for node.peers.Count(p2p.PeerInbound) != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := node.peers.Count(p2p.PeerInbound); got != 0 {
		t.Fatalf("the slot after the disconnected peer was not freed (%d)", got)
	}
}

// A node that is a DNS seed itself adds its own address from the seed. It has to remember it as
// its own after the first handshake — otherwise it knocks on itself every few seconds forever,
// with "a connection to ourselves" in its log each time.
func TestSelfAddressIsLearnedAndDropped(t *testing.T) {
	node, selfAddr, cleanup := listeningNode(t)
	defer cleanup()

	node.addrs.Add(selfAddr, net.ParseIP("88.198.113.1"))
	if newCount, _ := node.addrs.Size(); newCount != 1 {
		t.Fatalf("setup: expected 1 address, got %d", newCount)
	}

	// The first attempt has to fail on the self-connection detection…
	if err := node.FillOutboundOnce(p2p.PeerOutboundFull); err == nil {
		t.Fatal("a connection to ourselves should have failed")
	}
	// …and the address has to disappear from the tables.
	if !node.addrs.IsLocal(selfAddr) {
		t.Fatal("the address was not marked as our own")
	}
	if newCount, tried := node.addrs.Size(); newCount != 0 || tried != 0 {
		t.Fatalf("our own address stayed in the addrman (%d new, %d tried)", newCount, tried)
	}
	// Not even adding it again (for instance from another seed query) may bring it back.
	node.addrs.Add(selfAddr, net.ParseIP("88.198.113.1"))
	if newCount, _ := node.addrs.Size(); newCount != 0 {
		t.Fatalf("our own address came back into the addrman (%d)", newCount)
	}
}
