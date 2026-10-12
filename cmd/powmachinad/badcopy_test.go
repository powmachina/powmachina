package main

import (
	"net"
	"testing"

	"powmachina/internal/chain"
	"powmachina/internal/mh"
	"powmachina/internal/p2p"
)

// The id of a block covers the transactions without their witnesses. A peer that relays a block can
// flip a byte of a signature: the body still matches the header and fails validation. That must cost
// the copy and the peer, never the block — a node that marked the block invalid would refuse the
// honest chain from there on, across restarts, for the price of one connection.
func TestTamperedWitnessCostsTheCopyNotTheBlock(t *testing.T) {
	sk := new([32]byte)
	for i := range sk {
		sk[i] = byte(i + 3)
	}
	pk, err := mh.SchnorrPubkey(sk)
	if err != nil {
		t.Fatal(err)
	}
	op := chain.Outpoint{TxID: [32]byte{55}, Index: 0}
	coin := chain.Coin{Value: 1_000_000, Script: pk[:], Height: 0}

	src := NewNode(chain.RegTest, chain.RegtestGenesis(), mh.TestProfile)
	defer src.Close()
	node := NewNode(chain.RegTest, chain.RegtestGenesis(), mh.TestProfile)
	defer node.Close()
	for _, n := range []*Node{src, node} {
		if err := n.store.AddCoin(op, coin); err != nil {
			t.Fatal(err)
		}
	}
	if err := src.MineBlocks(2); err != nil {
		t.Fatal(err)
	}
	feedBranch(t, node, src, 1, 2)

	tx := &chain.Tx{Version: 1,
		Inputs:  []chain.TxIn{{PrevTxID: op.TxID, PrevIndex: op.Index}},
		Outputs: []chain.TxOut{{Value: 1_000_000, Script: pk[:]}}}
	sig, err := chain.SignInput(tx, 0, []chain.Coin{coin}, sk)
	if err != nil {
		t.Fatal(err)
	}
	tx.Witness = []chain.Witness{{Items: [][]byte{sig}}}
	good := mineBlockWith(t, src, []*chain.Tx{tx})
	id := good.Header.ID()

	tampered, err := chain.DeserializeBlock(good.Serialize())
	if err != nil {
		t.Fatal(err)
	}
	tampered.Txs[1].Witness[0].Items[0][10] ^= 1
	if tampered.Header.ID() != id || chain.TxMerkleRoot(tampered.Txs) != tampered.Header.MerkleRoot {
		t.Fatal("setup: the tampered copy no longer matches the header")
	}

	liar := p2p.NewPeer(p2p.Address{IP: net.ParseIP("203.0.113.7"), Port: 1}, p2p.PeerInbound)
	honest := p2p.NewPeer(p2p.Address{IP: net.ParseIP("203.0.113.8"), Port: 1}, p2p.PeerInbound)
	if _, err := node.acceptHeaders(liar, []*chain.Header{good.Header}); err != nil {
		t.Fatal(err)
	}
	if err := node.acceptBlock(liar, tampered); err != nil {
		t.Fatalf("a tampered copy must be dropped, not returned as an error: %v", err)
	}
	if node.store.IsInvalid(id) {
		t.Fatal("the block was marked invalid for a copy of its body")
	}
	if _, h, tip := node.chain.Tip(); h != 3 || tip != id {
		t.Fatalf("the header tip moved off the block: height %d", h)
	}
	if node.store.HasBody(id) {
		t.Fatal("the tampered body is still stored — it would be served to peers and never asked for again")
	}
	if applied, _ := node.appliedHeight(); applied != 2 {
		t.Fatalf("applied height is %d, want 2", applied)
	}
	if liar.BanScore() != p2p.MisbehaviorBadCopy {
		t.Fatalf("the peer that sent the tampered copy has a ban score of %d, want %d", liar.BanScore(), p2p.MisbehaviorBadCopy)
	}

	// The body as the miner made it, from somebody else: the block applies.
	if err := node.acceptBlock(honest, good); err != nil {
		t.Fatal(err)
	}
	if applied, _ := node.appliedHeight(); applied != 3 {
		t.Fatalf("the honest copy was not applied: height %d", applied)
	}
	if honest.BanScore() != 0 {
		t.Fatalf("the honest peer has a ban score of %d", honest.BanScore())
	}
}

// What the header commits to is still the block's own: a block that spends a coin nobody has is invalid
// whatever its witnesses, and the committed part says so.
func TestCommittedPartTellsABadBlockFromABadCopy(t *testing.T) {
	src := NewNode(chain.RegTest, chain.RegtestGenesis(), mh.TestProfile)
	defer src.Close()
	if err := src.MineBlocks(1); err != nil {
		t.Fatal(err)
	}
	bad := mineBlockWith(t, src, []*chain.Tx{spendNothing()})
	if err := bad.ValidateCommitted(src.store, src.params); err == nil {
		t.Fatal("a block that spends a missing coin has a valid committed part")
	}
}
