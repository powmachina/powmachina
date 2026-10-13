package main

import (
	"crypto/rand"
	"errors"
	"testing"
	"time"

	"powmachina/internal/chain"
	"powmachina/internal/mh"
	"powmachina/internal/p2p"
)

// mineBlockWith builds a block on the given node's tip with the supplied transactions and
// finds its PoW.
//
// It is the same thing MineBlocks does, but without checking the contents — precisely so that
// a block with **valid PoW and an invalid body** can be constructed. Anybody with hashrate can
// produce such a block and the node has to survive it.
func mineBlockWith(t *testing.T, src *Node, txs []*chain.Tx) *chain.Block {
	t.Helper()
	tip, height, tipID := src.chain.Tip()
	lanes := src.chain.AllowedLanesNow(time.Now())
	if len(lanes) == 0 {
		t.Fatal("no lane is allowed")
	}
	lane := lanes[0]
	for _, l := range lanes {
		if l == mh.LaneG { // on regtest lane G is orders of magnitude cheaper than RandomX
			lane = l
			break
		}
	}
	bits, err := src.chain.NextBitsFor(lane)
	if err != nil {
		t.Fatal(err)
	}
	var extra [32]byte
	if _, err := rand.Read(extra[:]); err != nil {
		t.Fatal(err)
	}
	cb := chain.BuildCoinbase(height+1, 0, src.minerKey, extra, src.params)
	blk := &chain.Block{
		Header: &chain.Header{Version: 1, PrevBlock: tipID, Bits: bits,
			Height: uint32(height + 1), Lane: lane},
		Txs: append([]*chain.Tx{cb}, txs...),
	}
	chain.SealCoinbase(blk.Txs, height+1, src.params)
	pow, err := src.pow(height + 1)
	if err != nil {
		t.Fatal(err)
	}
	h := blk.Header
	h.MerkleRoot = chain.TxMerkleRoot(blk.Txs)
	h.Time = uint32(time.Now().Unix())
	if low := src.chain.MinTime(); h.Time < low {
		h.Time = low
	}
	if h.Time <= tip.Time {
		h.Time = tip.Time + 1
	}
	for nonce := uint64(0); nonce < 5_000_000; nonce++ {
		h.Nonce = nonce
		ok, err := pow.VerifyPoW(h)
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			return blk
		}
	}
	t.Fatal("no nonce found")
	return nil
}

// spendNothing is a transaction that spends an output that does not exist. The header knows
// nothing about it — the merkle root matches, the PoW matches; the invalidity only shows up on
// validation against the UTXO set.
func spendNothing() *chain.Tx {
	return &chain.Tx{Version: 1,
		Inputs:  []chain.TxIn{{PrevTxID: [32]byte{0xDE, 0xAD}, PrevIndex: 0}},
		Outputs: []chain.TxOut{{Value: 1000, Script: make([]byte, 32)}},
		Witness: []chain.Witness{{Items: [][]byte{make([]byte, 64)}}}}
}

// A block with valid PoW and an invalid body must not stop the node: the branch is marked
// invalid, the chain stays at the last valid height and further valid blocks are applied.
//
// Without that, applying stops at that height for good — and since such a header spreads like
// any other, the whole network stops. The cost to the attacker is one block of work.
func TestInvalidBodyDoesNotStallNode(t *testing.T) {
	src := NewNode(chain.RegTest, chain.RegtestGenesis(), mh.TestProfile)
	defer src.Close()
	if err := src.MineBlocks(2); err != nil {
		t.Fatal(err)
	}

	node := NewNode(chain.RegTest, chain.RegtestGenesis(), mh.TestProfile)
	defer node.Close()
	feedBranch(t, node, src, 1, 2)
	_, good, goodID := node.chain.Tip()
	if good != 2 {
		t.Fatalf("setup: expected height 2, got %d", good)
	}

	bad := mineBlockWith(t, src, []*chain.Tx{spendNothing()})
	peer := p2p.NewPeer(p2p.Address{}, p2p.PeerInbound)
	if _, err := node.acceptHeaders(peer, []*chain.Header{bad.Header}); err != nil {
		t.Fatalf("the invalid block's header should have passed (it is formally fine): %v", err)
	}
	if _, h, _ := node.chain.Tip(); h != 3 {
		t.Fatalf("the header should have been filed, the tip is %d", h)
	}
	if err := node.acceptBlock(peer, bad); err != nil {
		t.Fatalf("the body must not return an error, it must drop the branch: %v", err)
	}

	// The chain has to return to the last valid height…
	_, h, id := node.chain.Tip()
	if h != good || id != goodID {
		t.Fatalf("the tip is %d (%x), expected %d (%x)", h, id[:6], good, goodID[:6])
	}
	if !node.chain.IsInvalid(bad.Header.ID()) {
		t.Fatal("the block is not marked invalid")
	}
	// …the invalid block's body must not stay in storage (otherwise the node hands it out to peers)…
	if node.store.HasBody(bad.Header.ID()) {
		t.Fatal("the invalid block's body stayed in storage")
	}
	// …the same header must not become the tip again (we know it in memory, so it is a
	// duplicate — what matters is that the tip does not move)…
	if _, err := node.chain.AddHeader(bad.Header, nil); !errors.Is(err, chain.ErrDuplicate) {
		t.Fatalf("a second submission should be a duplicate, got %v", err)
	}
	if _, h2, _ := node.chain.Tip(); h2 != good {
		t.Fatalf("the tip moved to %d after the second submission", h2)
	}
	// …and anything growing out of the invalid block is refused before the expensive checks
	// (that is, without computing PoW — the attack cannot be repeated for free).
	child := &chain.Header{Version: 1, PrevBlock: bad.Header.ID(), Height: bad.Header.Height + 1,
		Lane: bad.Header.Lane, Bits: bad.Header.Bits, Time: bad.Header.Time + 1}
	if _, err := node.chain.AddHeader(child, nil); !errors.Is(err, chain.ErrInvalidAncestor) {
		t.Fatalf("a child of the invalid block should have been refused, got %v", err)
	}
	// …and the node has to continue normally on the valid branch.
	if err := src.MineBlocks(1); err != nil {
		t.Fatal(err)
	}
	feedBranch(t, node, src, 3, 3)
	if _, h, _ := node.chain.Tip(); h != 3 {
		t.Fatalf("the node did not continue on the valid branch, the tip is %d", h)
	}
	tip, err := node.store.Tip()
	if err != nil || tip.Height != 3 {
		t.Fatalf("the UTXO set did not advance to 3: %+v (%v)", tip, err)
	}
}

// The mark on an invalid branch has to survive a restart — otherwise the node accepts the same
// header again after startup, downloads the body again and stalls again.
func TestInvalidBranchSurvivesRestart(t *testing.T) {
	src := NewNode(chain.RegTest, chain.RegtestGenesis(), mh.TestProfile)
	defer src.Close()
	if err := src.MineBlocks(2); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	node, _, err := OpenNode(chain.RegTest, chain.RegtestGenesis(), mh.TestProfile, dir)
	if err != nil {
		t.Fatal(err)
	}
	feedBranch(t, node, src, 1, 2)

	bad := mineBlockWith(t, src, []*chain.Tx{spendNothing()})
	peer := p2p.NewPeer(p2p.Address{}, p2p.PeerInbound)
	if _, err := node.acceptHeaders(peer, []*chain.Header{bad.Header}); err != nil {
		t.Fatal(err)
	}
	if err := node.acceptBlock(peer, bad); err != nil {
		t.Fatal(err)
	}
	badID := bad.Header.ID()
	if !node.chain.IsInvalid(badID) {
		t.Fatal("the block is not marked invalid")
	}
	node.Close()

	again, height, err := OpenNode(chain.RegTest, chain.RegtestGenesis(), mh.TestProfile, dir)
	if err != nil {
		t.Fatalf("the restart failed: %v", err)
	}
	defer again.Close()
	if height != 2 {
		t.Fatalf("height %d after the restart, expected 2", height)
	}
	if !again.chain.IsInvalid(badID) {
		t.Fatal("the invalid-branch mark did not survive the restart")
	}
	if _, err := again.chain.AddHeader(bad.Header, nil); !errors.Is(err, chain.ErrInvalidAncestor) {
		t.Fatalf("the header was accepted again after the restart: %v", err)
	}
}
