package main

import (
	"errors"
	"testing"
	"time"

	"powmachina/internal/chain"
	"powmachina/internal/mh"
	"powmachina/internal/p2p"
)

// shareOn is a share a miner with `payout` found on the node's tip, in lane G. On regtest the
// target is so loose that every hash is a share, so any nonce does.
func shareOn(t *testing.T, n *Node, payout byte, nonce uint64) *chain.Share {
	t.Helper()
	tip, _, tipID := n.chain.Tip()
	bits, err := n.chain.NextBitsFor(mh.LaneG)
	if err != nil {
		t.Fatal(err)
	}
	s := &chain.Share{Header: chain.Header{Version: 1, PrevBlock: tipID, Bits: bits, Height: tip.Height + 1,
		Time: tip.Time + 1, Nonce: nonce, Lane: mh.LaneG}, BodyRoot: [32]byte{payout}}
	s.Commit.Payout = [32]byte{payout}
	s.Header.MerkleRoot = chain.ShareRoot(s.BodyRoot, &s.Commit)
	return s
}

// A block pays the shares it includes once: the next block on the branch cannot include them
// again, also after the node forgot what it settled and has to read it back from the bodies.
func TestNodeSettlesAShareOnce(t *testing.T) {
	node := NewNode(chain.RegTest, chain.RegtestGenesis(), mh.TestProfile)
	defer node.Close()
	if err := node.MineBlocks(4); err != nil {
		t.Fatal(err)
	}
	a, b := shareOn(t, node, 1, 11), shareOn(t, node, 2, 12)
	if _, err := node.chain.CheckShare(a, epochHasher{node.epochs}); err != nil {
		t.Fatalf("the share as relayed: %v", err)
	}
	if err := node.SubmitBlock(mineBlockWith(t, node, nil, a, b)); err != nil {
		t.Fatalf("a block settling two shares: %v", err)
	}

	for _, forget := range []bool{false, true} {
		if forget {
			node.settled = settledIndex{}
		}
		err := node.SubmitBlock(mineBlockWith(t, node, nil, a))
		if !errors.Is(err, chain.ErrShareSettled) {
			t.Fatalf("forgot=%v: a share settled one block below: %v, expected ErrShareSettled", forget, err)
		}
	}
	// A share found later on the same parent is new work, and the next block takes it.
	c := shareOn(t, node, 3, 13)
	if err := node.SubmitBlock(mineBlockWith(t, node, nil, c)); err != nil {
		t.Fatalf("a new share: %v", err)
	}
}

// testPeer connects to the node as a bare peer that holds the full DAG and finishes the handshake.
func testPeer(t *testing.T, node *Node, addr string, nonce uint64) *p2p.Conn {
	t.Helper()
	conn, err := p2p.Dial(addr, node.params.Magic)
	if err != nil {
		t.Fatal(err)
	}
	ver := &p2p.VersionMsg{Protocol: p2p.ProtocolVersion, Timestamp: time.Now().Unix(), Nonce: nonce,
		UserAgent: "/test/", Services: p2p.ServiceFullDag}
	if _, err := conn.Handshake(ver, nonce, true); err != nil {
		t.Fatal(err)
	}
	return conn
}

// A share from a peer is verified, pooled and pushed to the other peers; the node's next block
// settles it, and after that no template offers it again.
func TestNodeRelaysAndSettlesAShare(t *testing.T) {
	node := NewNode(chain.RegTest, chain.RegtestGenesis(), mh.TestProfile)
	defer node.Close()
	if err := node.MineBlocks(4); err != nil {
		t.Fatal(err)
	}
	ln, err := p2p.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go node.Listen(ln)
	from := testPeer(t, node, ln.Addr().String(), 0xA1)
	defer from.Close()
	to := testPeer(t, node, ln.Addr().String(), 0xB2)
	defer to.Close()

	got := make(chan *chain.Share, 1)
	go func() {
		dec := p2p.NewShareDecoder()
		for {
			msg, err := to.Recv()
			if err != nil {
				return
			}
			if msg.Command == p2p.CmdShares {
				if shares, err := dec.Decode(msg.Payload); err == nil && len(shares) > 0 {
					got <- shares[0]
					return
				}
			}
		}
	}()

	s := shareOn(t, node, 7, 21)
	bad := shareOn(t, node, 8, 22)
	bad.Header.Bits ^= 1 // not the difficulty of a block on its parent
	enc := p2p.NewShareEncoder()
	for _, sh := range []*chain.Share{bad, s} {
		if err := from.Send(&p2p.Message{Command: p2p.CmdShares, Payload: enc.Encode([]*chain.Share{sh})}); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case r := <-got:
		if r.ID() != s.ID() {
			t.Fatal("the node relayed another share")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the share was not relayed")
	}
	if node.shares.Has(bad.ID()) {
		t.Fatal("a share with the wrong difficulty was pooled")
	}

	tpl, err := node.Template(mh.LaneG)
	if err != nil {
		t.Fatal(err)
	}
	if tpl.Shares != 1 {
		t.Fatalf("the template settles %d shares, expected 1", tpl.Shares)
	}
	if err := node.MineBlocks(1); err != nil {
		t.Fatal(err)
	}
	_, _, tipID := node.chain.Tip()
	blk, err := node.store.Block(tipID)
	if err != nil {
		t.Fatal(err)
	}
	if len(blk.Settlement.Shares) != 1 {
		t.Fatalf("the mined block settles %d shares, expected 1", len(blk.Settlement.Shares))
	}
	if tpl, err = node.Template(mh.LaneC); err != nil || tpl.Shares != 0 {
		t.Fatalf("after it was settled the template offers %d shares (%v)", tpl.Shares, err)
	}
}

// A light node verifies no shares: it neither pools nor relays them, and it still accepts a block
// that settles some, taking their PoW on trust.
func TestLightNodeTakesNoShares(t *testing.T) {
	node := NewNode(chain.RegTest, chain.RegtestGenesis(), mh.TestProfile)
	defer node.Close()
	if err := node.MineBlocks(4); err != nil {
		t.Fatal(err)
	}
	node.epochs.full.Store(false)
	s := shareOn(t, node, 5, 31)
	if _, err := node.SubmitShare(s); !errors.Is(err, errLightNode) || node.shares.Len() != 0 {
		t.Fatalf("a light node took a share: %v, %d pooled", err, node.shares.Len())
	}
	if err := node.SubmitBlock(mineBlockWith(t, node, nil, s)); err != nil {
		t.Fatalf("a light node refused a block settling a share: %v", err)
	}
}

// A share that overtakes its block waits for it, and is taken when the block comes.
func TestShareBeforeItsBlockWaitsForIt(t *testing.T) {
	a := NewNode(chain.RegTest, chain.RegtestGenesis(), mh.TestProfile)
	defer a.Close()
	b := NewNode(chain.RegTest, chain.RegtestGenesis(), mh.TestProfile)
	defer b.Close()
	if err := a.MineBlocks(1); err != nil {
		t.Fatal(err)
	}
	_, _, tipID := a.chain.Tip()
	blk, err := a.store.Block(tipID)
	if err != nil {
		t.Fatal(err)
	}
	s := shareOn(t, a, 7, 77)
	if err := b.acceptShare(s, "198.51.100.7:17696", "198.51.100.7:17696"); !errors.Is(err, chain.ErrUnknownParent) {
		t.Fatalf("a share on a block b does not have: %v", err)
	}
	if b.shareOrphans.len() != 1 {
		t.Fatal("the share is not waiting")
	}
	if err := b.SubmitBlock(blk); err != nil {
		t.Fatalf("the block: %v", err)
	}
	for deadline := time.Now().Add(10 * time.Second); !b.shares.Has(s.ID()); {
		if time.Now().After(deadline) {
			t.Fatal("the share was not taken when its block came")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if b.shareOrphans.len() != 0 {
		t.Fatal("the share is still waiting")
	}
}

// The waiting room is bounded per peer.
func TestOrphanSharesAreBounded(t *testing.T) {
	var o shareOrphans
	now := time.Now()
	for i := 0; i < maxOrphanSharesPeer+10; i++ {
		s := &chain.Share{Header: chain.Header{PrevBlock: [32]byte{byte(i), byte(i >> 8)}}}
		o.add(s, "peer", "peer", now)
	}
	if o.len() != maxOrphanSharesPeer {
		t.Fatalf("%d shares wait from one peer", o.len())
	}
	if !o.add(&chain.Share{}, "other", "other", now) {
		t.Fatal("another peer found no room")
	}
	// After a minute the old ones make room.
	if !o.add(&chain.Share{}, "peer", "peer", now.Add(2*orphanShareTTL)) {
		t.Fatal("expired shares did not make room")
	}
}
