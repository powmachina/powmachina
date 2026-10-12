package main

import (
	"testing"
	"time"

	"powmachina/internal/chain"
	"powmachina/internal/mh"
	"powmachina/internal/p2p"
)

// A miner announces a header and keeps the body. The header is valid, so the tip moves onto it — and
// nothing can be applied there, nor can any block built on it. Without parking, the chain stands for
// as long as the body is withheld, on every node, for the price of delaying one's own reward.
func TestWithheldBodyDoesNotStopTheChain(t *testing.T) {
	withholder := NewNode(chain.RegTest, chain.RegtestGenesis(), mh.TestProfile)
	defer withholder.Close()
	if err := withholder.MineBlocks(2); err != nil { // the part of the chain everybody has
		t.Fatal(err)
	}

	node, addr, cleanup := listeningNode(t)
	defer cleanup()
	rawPeer(t, addr, 0xFEEDFACE) // somebody to ask — a node with no peers parks nothing
	deadline := time.Now().Add(5 * time.Second)
	for len(node.connSnapshot()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the peer never connected")
		}
		time.Sleep(10 * time.Millisecond)
	}
	feedBranch(t, node, withholder, 1, 2)

	if err := withholder.MineBlocks(1); err != nil {
		t.Fatal(err)
	}
	hdr, _, withheld := withholder.chain.Tip()
	peer := p2p.NewPeer(p2p.Address{}, p2p.PeerInbound)
	if _, err := node.acceptHeaders(peer, []*chain.Header{hdr}); err != nil {
		t.Fatal(err)
	}
	if _, h, _ := node.chain.Tip(); h != 3 {
		t.Fatalf("setup: the header tip is %d, want 3", h)
	}

	now := time.Now()
	node.SyncBodiesOnce(now)
	// While a batch of headers is being verified its connection can deliver nothing else: that
	// time does not count, and the wait starts again when the batch is done.
	node.verifyingHeaders.Add(1)
	node.SyncBodiesOnce(now.Add(2 * bodyParkAfter))
	node.verifyingHeaders.Add(-1)
	if node.chain.IsParked(withheld) {
		t.Fatal("the block was parked while headers were being verified")
	}
	now = now.Add(2 * bodyParkAfter)
	node.SyncBodiesOnce(now)
	node.SyncBodiesOnce(now.Add(bodyParkAfter - time.Second))
	if node.chain.IsParked(withheld) {
		t.Fatal("the block was parked before bodyParkAfter")
	}
	node.SyncBodiesOnce(now.Add(bodyParkAfter + time.Second))
	if !node.chain.IsParked(withheld) {
		t.Fatal("a block whose body never came was not parked")
	}
	if _, h, _ := node.chain.Tip(); h != 2 {
		t.Fatalf("the tip is at %d, want it back at 2", h)
	}

	// Mining goes on beside the parked block, and what is found is applied.
	if err := node.MineBlocks(1); err != nil {
		t.Fatalf("mining beside a parked block: %v", err)
	}
	if applied, _ := node.appliedHeight(); applied != 3 {
		t.Fatalf("applied height is %d, want 3", applied)
	}
	_, _, ours := node.chain.Tip()

	// The body turns up after all. The block is back in the running, but it is no heavier than
	// the block mined meanwhile, so the tip stays.
	body, err := withholder.store.Block(withheld)
	if err != nil {
		t.Fatal(err)
	}
	if err := node.acceptBlock(peer, body); err != nil {
		t.Fatal(err)
	}
	if node.chain.IsParked(withheld) {
		t.Fatal("the block is still parked with its body in hand")
	}
	if _, _, tip := node.chain.Tip(); tip != ours {
		t.Fatal("a late body of equal work took the tip")
	}

	// The withholder's branch grows heavier: now it wins, like any other branch would.
	if err := withholder.MineBlocks(1); err != nil {
		t.Fatal(err)
	}
	feedBranch(t, node, withholder, 4, 4)
	if _, h, tip := node.chain.Tip(); h != 4 || tip != mustTipID(t, withholder) {
		t.Fatalf("the heavier branch was not taken: height %d", h)
	}
	if applied, _ := node.appliedHeight(); applied != 4 {
		t.Fatalf("applied height is %d, want 4", applied)
	}
}

// A header that grows out of a parked block is kept, and the whole branch comes back with the body.
func TestHeadersAboveAParkedBlockWaitWithIt(t *testing.T) {
	src := NewNode(chain.RegTest, chain.RegtestGenesis(), mh.TestProfile)
	defer src.Close()
	if err := src.MineBlocks(3); err != nil {
		t.Fatal(err)
	}
	node := NewNode(chain.RegTest, chain.RegtestGenesis(), mh.TestProfile)
	defer node.Close()
	peer := p2p.NewPeer(p2p.Address{}, p2p.PeerInbound)

	var hdrs []*chain.Header
	for h := uint64(1); h <= 3; h++ {
		id, _ := src.chain.BlockIDAt(h)
		hdr, _ := src.chain.HeaderByID(id)
		hdrs = append(hdrs, hdr)
	}
	if _, err := node.acceptHeaders(peer, hdrs[:1]); err != nil {
		t.Fatal(err)
	}
	first := hdrs[0].ID()
	node.parkBlock(first)
	if added, err := node.acceptHeaders(peer, hdrs[1:]); err != nil || added != 2 {
		t.Fatalf("headers above a parked block: added %d, err %v", added, err)
	}
	if _, h, _ := node.chain.Tip(); h != 0 {
		t.Fatalf("a header above a parked block moved the tip to %d", h)
	}
	for h := uint64(1); h <= 3; h++ {
		id, _ := src.chain.BlockIDAt(h)
		body, err := src.store.Block(id)
		if err != nil {
			t.Fatal(err)
		}
		if err := node.acceptBlock(peer, body); err != nil {
			t.Fatal(err)
		}
	}
	if applied, _ := node.appliedHeight(); applied != 3 {
		t.Fatalf("applied height is %d, want 3", applied)
	}
}
