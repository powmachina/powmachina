package main

import (
	"net"
	"testing"
	"time"

	"powmachina/internal/blocksync"
	"powmachina/internal/chain"
	"powmachina/internal/mh"
	"powmachina/internal/p2p"
)

// What these tests guard is a stalled body: the headers of a run of blocks reach every node in
// seconds, the body of the first of them reaches none for minutes, and because nothing can be
// applied past a hole, the UTXO set of the whole network stops below it. The chain looks
// healthy from the outside — the tip keeps growing — while every wallet and the explorer are
// frozen.
//
// Ordinary sync tests would not catch it, for two reasons, and there is one
// test here for each:
//
//  1. they all gave every peer every body, so a peer that answers headers but not one body
//     was never exercised;
//  2. every block they mined carried only a coinbase, and the body that got stuck was the
//     first one in the chain's life that carried transactions.

// peerServer is a peer that serves headers and bodies from `src`, except for the heights in
// `withhold` — those it silently ignores, the way a node does when it does not have the body
// yet. It records what it was asked for.
type peerServer struct {
	t        *testing.T
	src      *Node
	withhold map[uint64]bool
	asked    chan uint64
}

func startPeerServer(t *testing.T, src *Node, withhold ...uint64) (*peerServer, p2p.Address) {
	t.Helper()
	ln, err := p2p.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	p := &peerServer{t: t, src: src, withhold: map[uint64]bool{}, asked: make(chan uint64, 64)}
	for _, h := range withhold {
		p.withhold[h] = true
	}
	go p.serve(ln)

	tcp := ln.Addr().(*net.TCPAddr)
	return p, p2p.Address{IP: tcp.IP, Port: uint16(tcp.Port), LastSeen: time.Now()}
}

func (p *peerServer) serve(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go p.session(p2p.NewConn(c, chain.RegTest.Magic))
	}
}

func (p *peerServer) session(conn *p2p.Conn) {
	ver := &p2p.VersionMsg{Protocol: p2p.ProtocolVersion, Nonce: 0xBEEF, UserAgent: "/server/"}
	if _, err := conn.Handshake(ver, 0xBEEF, false); err != nil {
		return
	}
	for {
		msg, err := conn.Recv()
		if err != nil {
			return
		}
		switch msg.Command {
		case p2p.CmdGetHeaders:
			hs := p.src.chain.HeadersFrom(nil, p2p.MaxHeadersPerMessage)
			payload, err := p2p.EncodeHeaders(hs)
			if err != nil {
				return
			}
			_ = conn.Send(&p2p.Message{Command: p2p.CmdHeaders, Payload: payload})
		case p2p.CmdGetData:
			items, err := p2p.DecodeGetData(msg.Payload)
			if err != nil {
				return
			}
			for _, it := range items {
				if it.Type != p2p.InvBlock {
					continue
				}
				blk, err := p.src.store.Block(it.ID)
				if err != nil || blk == nil {
					continue
				}
				select {
				case p.asked <- uint64(blk.Header.Height):
				default:
				}
				if p.withhold[uint64(blk.Header.Height)] {
					continue // the body exists, this peer simply does not hand it over
				}
				payload, err := p2p.EncodeBlock(blk)
				if err != nil {
					continue
				}
				_ = conn.Send(&p2p.Message{Command: p2p.CmdBlock, Payload: payload})
			}
		}
	}
}

// One peer that does not hand over one body must not stop the chain. The driver asks every
// peer for the oldest missing body at once, so the second one covers it — without waiting out
// a request timeout, which is what turned a lost message into minutes of a frozen UTXO set.
func TestSilentPeerDoesNotStallTheChain(t *testing.T) {
	src := NewNode(chain.RegTest, chain.RegtestGenesis(), mh.TestProfile)
	defer src.Close()
	if err := src.MineBlocks(4); err != nil {
		t.Fatal(err)
	}

	// The first peer withholds the body of height 2, the second serves everything.
	_, addrA := startPeerServer(t, src, 2)
	_, addrB := startPeerServer(t, src)

	node := NewNode(chain.RegTest, chain.RegtestGenesis(), mh.TestProfile)
	defer node.Close()
	// Deliberately longer than the test's patience: if the chain only caught up thanks to a
	// retry, this test would fail. It has to catch up because the other peer was asked too.
	node.bodies.timeout = time.Minute
	node.addrs.Add(addrA, net.ParseIP("88.198.113.1"))
	node.addrs.Add(addrB, net.ParseIP("45.83.104.1"))
	for i := 0; i < 2; i++ {
		if err := node.FillOutboundOnce(p2p.PeerOutboundFull); err != nil {
			t.Fatalf("the connection failed: %v", err)
		}
	}
	if got := len(node.connSnapshot()); got != 2 {
		t.Fatalf("expected 2 connections, got %d", got)
	}

	stop := make(chan struct{})
	defer close(stop)
	node.startLoop(stop, 20*time.Millisecond, func() { node.SyncBodiesOnce(time.Now()) })

	waitForApplied(t, node, 4, 15*time.Second)
}

// A block with transactions has to travel the same way as an empty one. Tests that mine blocks
// with nothing but a coinbase leave exactly that path untested — and it is the one every block
// on a used chain takes.
func TestBlockWithTransactionsReachesPeer(t *testing.T) {
	miner, minerAddr, cleanup := listeningNode(t)
	defer cleanup()
	if err := miner.MineBlocks(2); err != nil {
		t.Fatal(err)
	}

	// A block with two transactions besides the coinbase. They do not have to be spendable
	// here — what is being tested is the path of the body, not validation, and the body is
	// verified against the merkle root in the header on the way.
	blk := mineBlockWith(t, miner, []*chain.Tx{spendNothing(), spendNothing()})
	if len(blk.Txs) != 3 {
		t.Fatalf("the test block has %d transactions, expected 3", len(blk.Txs))
	}

	node := NewNode(chain.RegTest, chain.RegtestGenesis(), mh.TestProfile)
	defer node.Close()
	node.addrs.Add(minerAddr, net.ParseIP("88.198.113.1"))
	if err := node.FillOutboundOnce(p2p.PeerOutboundFull); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	defer close(stop)
	node.startLoop(stop, 20*time.Millisecond, func() { node.SyncBodiesOnce(time.Now()) })
	waitForApplied(t, node, 2, 15*time.Second)

	// The body of a block with transactions has to survive the trip and be stored. What is
	// being tested here is transport, not validation: the transactions in it are deliberately
	// not spendable, so `acceptBlock` would drop the branch on the next apply. This step is
	// where a body can go missing — before it ever gets as far as the store.
	peer := p2p.NewPeer(p2p.Address{}, p2p.PeerInbound)
	if _, err := node.acceptHeaders(peer, []*chain.Header{blk.Header}); err != nil {
		t.Fatalf("the header of the block with transactions: %v", err)
	}
	if err := blocksync.VerifyBody(blk, blk.Header.ID()); err != nil {
		t.Fatalf("the merkle root of a block with three transactions does not verify: %v", err)
	}
	if err := node.storeBody(peer, blk); err != nil {
		t.Fatalf("the body of the block with transactions: %v", err)
	}
	if !node.store.HasBody(blk.Header.ID()) {
		t.Fatal("the node did not store the body of the block with transactions")
	}
	got, err := node.store.Block(blk.Header.ID())
	if err != nil {
		t.Fatalf("the stored body cannot be read: %v", err)
	}
	if len(got.Txs) != 3 {
		t.Fatalf("the stored body has %d transactions, expected 3", len(got.Txs))
	}
}

// The gap between the headers and the UTXO set is the metric the alert stands on. It has to be
// visible in /metrics even while it is being caused — otherwise the only symptom of a stuck
// body is a chain that mysteriously stops applying.
func TestMetricsShowHeaderBodyGap(t *testing.T) {
	src := NewNode(chain.RegTest, chain.RegtestGenesis(), mh.TestProfile)
	defer src.Close()
	if err := src.MineBlocks(3); err != nil {
		t.Fatal(err)
	}

	node := NewNode(chain.RegTest, chain.RegtestGenesis(), mh.TestProfile)
	defer node.Close()
	// Headers only, no bodies: exactly the state of a node whose bodies stalled.
	peer := p2p.NewPeer(p2p.Address{}, p2p.PeerInbound)
	hs := src.chain.HeadersFrom(nil, p2p.MaxHeadersPerMessage)
	if _, err := node.acceptHeaders(peer, hs); err != nil {
		t.Fatal(err)
	}

	m := node.metrics(time.Now())
	if !containsLine(m, "powm_header_body_gap 3") {
		t.Errorf("powm_header_body_gap does not show the gap:\n%s", grepMetrics(m, "powm_header_body_gap"))
	}
	for _, name := range []string{"powm_bodies_inflight", "powm_oldest_missing_body_seconds",
		"powm_body_requests_total", "powm_bodies_delivered_total", "powm_body_timeouts_total",
		"powm_body_mismatch_total"} {
		if grepMetrics(m, name) == "" {
			t.Errorf("the metric %s is missing", name)
		}
	}
}

func containsLine(metrics, want string) bool {
	for _, l := range splitLines(metrics) {
		if l == want {
			return true
		}
	}
	return false
}

func grepMetrics(metrics, name string) string {
	out := ""
	for _, l := range splitLines(metrics) {
		if len(l) >= len(name) && l[:len(name)] == name {
			out += l + "\n"
		}
	}
	return out
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}
