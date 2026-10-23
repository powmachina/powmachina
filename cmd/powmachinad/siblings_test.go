package main

import (
	"bytes"
	"errors"
	"net"
	"testing"

	"powmachina/internal/chain"
	"powmachina/internal/mh"
	"powmachina/internal/p2p"
)

// Two siblings: the node takes the lower ID, and a share on the other one worth more than the
// margin moves the tip and the UTXO set to it (the Sharepool spec §4).
func TestNodeSwitchesToTheSiblingTheSharesVoteFor(t *testing.T) {
	a := NewNode(chain.RegTest, chain.RegtestGenesis(), mh.TestProfile)
	defer a.Close()
	b := NewNode(chain.RegTest, chain.RegtestGenesis(), mh.TestProfile)
	defer b.Close()
	if err := a.MineBlocks(3); err != nil {
		t.Fatal(err)
	}
	for h := uint64(1); h <= 3; h++ {
		id, _ := a.chain.BlockIDAt(h)
		blk, err := a.store.Block(id)
		if err != nil {
			t.Fatal(err)
		}
		if err := b.SubmitBlock(blk); err != nil {
			t.Fatalf("copying block %d: %v", h, err)
		}
	}
	if err := a.MineBlocks(1); err != nil {
		t.Fatal(err)
	}
	if err := b.MineBlocks(1); err != nil {
		t.Fatal(err)
	}
	_, _, mine := a.chain.Tip()
	_, _, theirs := b.chain.Tip()
	// The difficulty of a share on each sibling, read while it is its node's tip.
	bitsOn := map[[32]byte]uint32{}
	for _, n := range []*Node{a, b} {
		_, _, id := n.chain.Tip()
		bits, err := n.chain.NextBitsFor(mh.LaneG)
		if err != nil {
			t.Fatal(err)
		}
		bitsOn[id] = bits
	}
	other, err := b.store.Block(theirs)
	if err != nil {
		t.Fatal(err)
	}
	peer := p2p.NewPeer(p2p.Address{IP: net.ParseIP("203.0.113.9"), Port: 1}, p2p.PeerInbound)
	if _, err := a.acceptHeaders(peer, []*chain.Header{other.Header}); err != nil {
		t.Fatal(err)
	}
	if err := a.acceptBlock(peer, other); err != nil {
		t.Fatal(err)
	}

	low, high := mine, theirs
	if bytes.Compare(theirs[:], mine[:]) < 0 {
		low, high = theirs, mine
	}
	if _, _, tip := a.chain.Tip(); tip != low {
		t.Fatalf("with no votes the tip is %x, want the lower ID %x", tip[:4], low[:4])
	}

	// A share on the higher sibling worth 1,024 units: more than the margin.
	parent, _ := a.chain.HeaderByID(high)
	bits := bitsOn[high]
	var voted bool
	for nonce := uint64(0); nonce < 1_000_000 && !voted; nonce++ {
		s := &chain.Share{Header: chain.Header{Version: 1, PrevBlock: high, Bits: bits, Height: parent.Height + 1,
			Time: parent.Time + 1, Nonce: nonce, Lane: mh.LaneG}}
		s.Commit.Payout = [32]byte{9}
		s.Commit.Weight = 10
		s.Header.MerkleRoot = chain.ShareRoot(s.BodyRoot, &s.Commit)
		switch _, err := a.SubmitShare(s); {
		case err == nil:
			voted = true
		case !errors.Is(err, chain.ErrSharePoW):
			t.Fatalf("the share: %v", err)
		}
	}
	if !voted {
		t.Fatal("no nonce met a weight-10 share")
	}
	if _, _, tip := a.chain.Tip(); tip != high {
		t.Fatalf("after the votes the tip is %x, want the voted sibling %x", tip[:4], high[:4])
	}
	if applied, _ := a.store.Tip(); applied.ID != high {
		t.Fatal("the UTXO set did not follow the tip to the voted sibling")
	}
}
