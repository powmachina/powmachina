package chain

import (
	"errors"
	"testing"
)

// An attestation we already have has to be refused by Add as a duplicate — that is how the node
// knows **not to relay it**. Without that, gossip between three or more nodes loops forever and
// floods the peers.
func TestAddRejectsDuplicateSoGossipStops(t *testing.T) {
	f := NewFinalityState(RegTest.SignerSet())
	att := Sign(RegtestSignerKey(0), 0, 100, [32]byte{1, 2, 3})

	if ev, err := f.Add(att); err != nil || ev != nil {
		t.Fatalf("the first attestation should have passed: %v, %v", ev, err)
	}
	for i := 0; i < 5; i++ {
		ev, err := f.Add(att)
		if !errors.Is(err, ErrDuplicateAttestation) {
			t.Fatalf("repeat %d: expected ErrDuplicateAttestation, got %v", i, err)
		}
		if ev != nil {
			t.Fatal("a duplicate is not proof of double signing")
		}
	}
	// And it still counts once.
	if n := len(f.byHeight[100]); n != 1 {
		t.Fatalf("the duplicates were counted: %d attestations", n)
	}
}

// Proof of double signing is produced once for every new conflicting attestation; its repeats go
// through as duplicates, so not even the proof travels in circles.
func TestEquivocationProofOncePerConflict(t *testing.T) {
	f := NewFinalityState(RegTest.SignerSet())
	first := Sign(RegtestSignerKey(1), 1, 200, [32]byte{0xAA})
	second := Sign(RegtestSignerKey(1), 1, 200, [32]byte{0xBB})

	if _, err := f.Add(first); err != nil {
		t.Fatal(err)
	}
	ev, err := f.Add(second)
	if ev == nil || !errors.Is(err, ErrSignerConflict) {
		t.Fatalf("the conflict was not recognized: %v, %v", ev, err)
	}
	ev2, err2 := f.Add(second)
	if ev2 != nil || !errors.Is(err2, ErrDuplicateAttestation) {
		t.Fatalf("a repeated conflict has to be a duplicate: %v, %v", ev2, err2)
	}
	if !f.disputed[200] {
		t.Fatal("height 200 should be disputed")
	}
}

// The state must not grow with the chain: what is below the finalized height is never recomputed.
func TestFinalityStatePrunesBelowFinalized(t *testing.T) {
	f := NewFinalityState(RegTest.SignerSet())
	const target = 50
	ids := map[uint64][32]byte{}
	for h := uint64(40); h <= target; h++ {
		var id [32]byte
		id[0] = byte(h)
		ids[h] = id
		for i := 0; i < FinalityThreshold; i++ {
			if _, err := f.Add(Sign(RegtestSignerKey(i), uint8(i), h, id)); err != nil {
				t.Fatal(err)
			}
		}
	}
	f.Update(target+FinalityDepth, func(h uint64) ([32]byte, bool) {
		id, ok := ids[h]
		return id, ok
	})
	if got, _, ok := f.Finalized(); !ok || got != target {
		t.Fatalf("finality is %d (%v), expected %d", got, ok, target)
	}
	for h := range f.byHeight {
		if h < target {
			t.Fatalf("height %d stayed in the state below the finalized %d", h, target)
		}
	}
	if len(f.seen) == 0 {
		t.Fatal("the state of the finalized height should not have been thrown away entirely")
	}
	for k := range f.seen {
		if k.height < target {
			t.Fatalf("the attestation of height %d stayed in the seen list below %d", k.height, target)
		}
	}
}
