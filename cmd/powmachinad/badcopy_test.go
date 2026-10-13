package main

import (
	"errors"
	"net"
	"testing"

	"powmachina/internal/chain"
	"powmachina/internal/mh"
	"powmachina/internal/p2p"
)

// spendScene is two nodes on the same chain of two blocks, a third block mined on the first node
// that spends a coin both have, and a copy of that block with one bit of the signature flipped.
// With badSignature the miner itself put a broken signature into the block.
func spendScene(t *testing.T, params chain.NetParams, badSignature bool) (*Node, *chain.Block, *chain.Block) {
	t.Helper()
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

	src := NewNode(params, chain.RegtestGenesis(), mh.TestProfile)
	t.Cleanup(src.Close)
	node := NewNode(params, chain.RegtestGenesis(), mh.TestProfile)
	t.Cleanup(node.Close)
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
	if badSignature {
		tx.Witness[0].Items[0][10] ^= 1
	}
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

	return node, good, tampered
}

// The id of a block covers the transactions without their witnesses. A peer that relays a block can
// flip a byte of a signature: the body still matches the header and fails validation. That must cost
// the copy and the peer, never the block — a node that marked the block invalid would refuse the
// honest chain from there on, across restarts, for the price of one connection.
//
// This is the path below the witness commitment fork, where a node cannot tell on arrival.
func TestTamperedWitnessCostsTheCopyNotTheBlock(t *testing.T) {
	before := chain.RegTest
	before.Forks = nil
	node, good, tampered := spendScene(t, before, false)
	id := good.Header.ID()

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
	// The block waits for a copy that validates; the tip does not sit on it in the meantime.
	if !node.chain.IsParked(id) || !node.chain.Has(id) {
		t.Fatal("the block should be parked, with its header kept")
	}
	if _, h, _ := node.chain.Tip(); h != 2 {
		t.Fatalf("the tip is at %d while the block has no valid body, want 2", h)
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
	if _, h, tip := node.chain.Tip(); h != 3 || tip != id || node.chain.IsParked(id) {
		t.Fatalf("the block did not get the tip back: height %d", h)
	}
	if honest.BanScore() != 0 {
		t.Fatalf("the honest peer has a ban score of %d", honest.BanScore())
	}
}

// With the commitment in the coinbase the node does not have to validate anything to know: the
// witnesses of the copy do not hash to what the block commits to. The body is turned away at the
// door — not stored, not validated, the block not parked — and the peer is gone.
func TestTamperedWitnessIsRefusedOnArrival(t *testing.T) {
	node, good, tampered := spendScene(t, chain.RegTest, false)
	id := good.Header.ID()

	liar := p2p.NewPeer(p2p.Address{IP: net.ParseIP("203.0.113.7"), Port: 1}, p2p.PeerInbound)
	honest := p2p.NewPeer(p2p.Address{IP: net.ParseIP("203.0.113.8"), Port: 1}, p2p.PeerInbound)
	if _, err := node.acceptHeaders(liar, []*chain.Header{good.Header}); err != nil {
		t.Fatal(err)
	}
	if err := node.acceptBlock(liar, tampered); !errors.Is(err, chain.ErrBadWitnessCommitment) {
		t.Fatalf("the tampered copy: %v, expected ErrBadWitnessCommitment", err)
	}
	if node.store.HasBody(id) {
		t.Fatal("the tampered body was stored")
	}
	if node.store.IsInvalid(id) || node.chain.IsParked(id) || !node.chain.Has(id) {
		t.Fatal("a refused copy must leave the block as it was: known, not invalid, not parked")
	}
	if applied, _ := node.appliedHeight(); applied != 2 {
		t.Fatalf("applied height is %d, want 2", applied)
	}
	if liar.BanScore() < p2p.BanThreshold {
		t.Fatalf("the peer that made the copy has a ban score of %d, want a ban", liar.BanScore())
	}

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

// The other half of the same rule: when the witnesses are the ones the block commits to and a
// signature in them is wrong, there is no better copy to wait for. The block is invalid — below
// the fork the node could not know that and would have parked it.
func TestCommittedBadSignatureIsTheBlocksFault(t *testing.T) {
	node, bad, _ := spendScene(t, chain.RegTest, true)
	id := bad.Header.ID()
	if err := bad.CheckWitnesses(node.params); err != nil {
		t.Fatalf("setup: the block does not match its own commitment: %v", err)
	}

	peer := p2p.NewPeer(p2p.Address{IP: net.ParseIP("203.0.113.9"), Port: 1}, p2p.PeerInbound)
	if _, err := node.acceptHeaders(peer, []*chain.Header{bad.Header}); err != nil {
		t.Fatal(err)
	}
	_ = node.acceptBlock(peer, bad)
	if !node.store.IsInvalid(id) {
		t.Fatal("a block that commits to a wrong signature was not marked invalid")
	}
	if node.chain.IsParked(id) {
		t.Fatal("the block is parked — the node is waiting for a copy that cannot exist")
	}
	if _, h, _ := node.chain.Tip(); h != 2 {
		t.Fatalf("the tip is at %d, want 2", h)
	}
	// The peer could not have known: it takes a UTXO set to see it.
	if peer.BanScore() >= p2p.BanThreshold {
		t.Fatalf("the peer that passed the block on was banned (%d)", peer.BanScore())
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

// The whole road of a fork on a running chain: before its height the blocks signal the rule and
// carry no commitment, from its height on they carry it and stop signalling — and a second node
// with the same table follows the chain across the height.
func TestChainCrossesTheWitnessCommitmentFork(t *testing.T) {
	const fork = 4
	params := chain.RegTest
	params.Forks = []chain.ForkAt{{Fork: chain.ForkWitnessCommitment, Height: fork}}

	src := NewNode(params, chain.RegtestGenesis(), mh.TestProfile)
	defer src.Close()
	if err := src.MineBlocks(fork + 2); err != nil {
		t.Fatal(err)
	}
	for h := uint64(1); h <= fork+2; h++ {
		id, _ := src.chain.BlockIDAt(h)
		b, err := src.store.Block(id)
		if err != nil {
			t.Fatal(err)
		}
		signals := chain.SignalsFork(b.Header.Version, chain.ForkWitnessCommitment)
		last := b.Txs[0].Outputs[len(b.Txs[0].Outputs)-1]
		commits := last.ScriptVersion == chain.ScriptVersionData && len(last.Script) == chain.WitnessCommitmentBytes
		if before := h < fork; signals != before || commits == before {
			t.Fatalf("height %d (fork at %d): signals %v, commits %v", h, fork, signals, commits)
		}
	}
	st := src.chain.ForkStatus()
	if len(st.Forks) != 1 || !st.Forks[0].Active || st.Forks[0].Height != fork {
		t.Fatalf("the fork status after the height: %+v", st.Forks)
	}
	if got := st.Forks[0].Signalling[mh.LaneC] + st.Forks[0].Signalling[mh.LaneG]; got != fork-1 {
		t.Fatalf("%d blocks signalled, expected the %d below the fork", got, fork-1)
	}

	node := NewNode(params, chain.RegtestGenesis(), mh.TestProfile)
	defer node.Close()
	feedBranch(t, node, src, 1, fork+2)
	if applied, _ := node.appliedHeight(); applied != fork+2 {
		t.Fatalf("the second node applied %d blocks of %d", applied, fork+2)
	}

	// A node whose table does not have the fork yet stops at the height: it does not seal, so
	// its own block there is not one the others take.
	stale := chain.RegTest
	stale.Forks = nil
	old := NewNode(stale, chain.RegtestGenesis(), mh.TestProfile)
	defer old.Close()
	feedBranch(t, old, src, 1, fork-1)
	if err := old.MineBlocks(1); err != nil {
		t.Fatal(err)
	}
	_, _, oldTip := old.chain.Tip()
	unsealed, err := old.store.Block(oldTip)
	if err != nil {
		t.Fatal(err)
	}
	if err := unsealed.ValidateBlock(node.store, params); !errors.Is(err, chain.ErrNoWitnessCommitment) {
		t.Fatalf("a block without a commitment at the fork height: %v", err)
	}
}
