package main

import (
	"strings"
	"testing"

	"powmachina/internal/chain"
)

// addAttestation computes the quorum for the attestation's height before it can check the signer's
// seat (the seat check needs that quorum). Computing a quorum walks a whole FinalityWindow and reads
// blocks from disk, so an unsolicited attestation must not reach it for an arbitrary height — a peer
// could otherwise spray far-apart heights and make the node burn CPU and disk at no cost. The gate
// drops, before any quorum, the heights the node would never act on: above the tip, at or below the
// finalized height, or more than a window+depth below the tip. A legitimate attestation at the
// pending height (tip − FinalityDepth) must still go through — including on regtest, where the depth
// is larger than the window.
func TestAttestationGateDropsOutOfBandHeights(t *testing.T) {
	node := quietNode(t)
	// A few blocks past the shortest finalizable chain, so a low height is more than a window+depth
	// below the tip while the pending height is still comfortably inside the band. quietNode does
	// not sign, so until the test files an attestation nothing is finalized yet.
	if err := node.MineBlocks(finalityBlocks + 5); err != nil {
		t.Fatal(err)
	}
	_, tip, _ := node.chain.Tip()

	// Far below the tip, before anything is finalized: a real, seated block, but more than a
	// window+depth down. Without the gate this would build the quorum and store the attestation;
	// the floor drops it first. (Once finality advances, such a height is caught earlier still, by
	// the finalized gate — see the end of the test.)
	const low = uint64(3)
	lowID, ok := node.chain.BlockIDAt(low)
	if !ok {
		t.Fatalf("no block at height %d", low)
	}
	if low+node.params.FinalityWindow+chain.FinalityDepth >= tip {
		t.Fatalf("setup: height %d is not far enough below tip %d to exercise the floor", low, tip)
	}
	if _, _, final := node.finality.Finalized(); final {
		t.Fatal("setup: nothing should be finalized before the test signs")
	}
	if err := node.addAttestation(chain.Sign(node.signer.Priv(), node.chain.GenesisID(), low, lowID), ""); err == nil ||
		!strings.Contains(err.Error(), "too far below") {
		t.Fatalf("an attestation far below the tip: expected the floor to drop it, got %v", err)
	}
	if got := node.finality.InRange(low, low, 10); len(got) != 0 {
		t.Fatalf("a far-below attestation was stored — the gate let it reach the quorum: %d", len(got))
	}

	// Above the tip: a block we do not have yet. Dropped before the quorum.
	if err := node.addAttestation(chain.Sign(node.signer.Priv(), node.chain.GenesisID(), tip+5, lowID), ""); err == nil ||
		!strings.Contains(err.Error(), "above our tip") {
		t.Fatalf("an attestation above the tip: expected the gate to drop it, got %v", err)
	}

	// The pending height, the node's own seated key: accepted and stored, and (as the whole quorum
	// of its own chain) finalized.
	target, id := pendingOf(t, node)
	if err := node.addAttestation(chain.Sign(node.signer.Priv(), node.chain.GenesisID(), target, id), ""); err != nil {
		t.Fatalf("a legitimate attestation at the pending height was refused: %v", err)
	}
	if got := node.finality.InRange(target, target, 10); len(got) != 1 {
		t.Fatalf("the legitimate attestation was not stored: %d", len(got))
	}

	// At or below the finalized height: refused as a duplicate, again without recomputing a quorum.
	fin, finID, ok := node.finality.Finalized()
	if !ok {
		t.Fatal("the pending-height attestation should have finalized the chain")
	}
	if err := node.addAttestation(chain.Sign(node.signer.Priv(), node.chain.GenesisID(), fin, finID), ""); err == nil ||
		!strings.Contains(err.Error(), "already have") {
		t.Fatalf("an attestation at the finalized height: expected a duplicate, got %v", err)
	}
}
