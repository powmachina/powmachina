package chain

import (
	"crypto/ed25519"
	"math"
	"slices"
	"testing"
	"time"
)

type quorum struct {
	set   *SignerSet
	privs []ed25519.PrivateKey
}

func newQuorum(t *testing.T, n, threshold int) *quorum {
	t.Helper()
	q := &quorum{set: &SignerSet{Threshold: threshold}}
	for i := 0; i < n; i++ {
		pub, priv, err := ed25519.GenerateKey(nil)
		if err != nil {
			t.Fatal(err)
		}
		q.set.Keys = append(q.set.Keys, pub)
		q.privs = append(q.privs, priv)
	}
	return q
}

func blockID(b byte) [32]byte {
	var id [32]byte
	for i := range id {
		id[i] = b
	}
	return id
}

// The chain the node really has.
func chainOf(ids map[uint64][32]byte) ChainLookup {
	return func(h uint64) ([32]byte, bool) {
		id, ok := ids[h]
		return id, ok
	}
}

func TestFinalityReachesThreshold(t *testing.T) {
	q := newQuorum(t, FinalitySigners, FinalityThreshold)
	f := NewFinalityState(q.set)
	ours := chainOf(map[uint64][32]byte{100: blockID(0xaa)})

	for i := 0; i < FinalityThreshold-1; i++ {
		if _, err := f.Add(Sign(q.privs[i], uint8(i), 100, blockID(0xaa))); err != nil {
			t.Fatal(err)
		}
	}
	f.Update(200, ours)
	if _, _, ok := f.Finalized(); ok {
		t.Fatal("nothing may be finalized below the threshold")
	}

	if _, err := f.Add(Sign(q.privs[FinalityThreshold-1], FinalityThreshold-1, 100, blockID(0xaa))); err != nil {
		t.Fatal(err)
	}
	f.Update(200, ours)
	h, id, ok := f.Finalized()
	if !ok || h != 100 || id != blockID(0xaa) {
		t.Fatalf("after reaching the threshold height 100 should be finalized, got %d %x ok=%v", h, id[:2], ok)
	}
}

// Only a block deep enough below the tip may be finalized.
func TestFinalityRespectsDepth(t *testing.T) {
	q := newQuorum(t, FinalitySigners, FinalityThreshold)
	f := NewFinalityState(q.set)
	ours := chainOf(map[uint64][32]byte{100: blockID(0xaa)})
	for i := 0; i < FinalityThreshold; i++ {
		f.Add(Sign(q.privs[i], uint8(i), 100, blockID(0xaa)))
	}

	f.Update(100+FinalityDepth-1, ours)
	if _, _, ok := f.Finalized(); ok {
		t.Fatal("a block closer than FinalityDepth must not be finalized")
	}
	f.Update(100+FinalityDepth, ours)
	if _, _, ok := f.Finalized(); !ok {
		t.Fatal("at depth FinalityDepth it should already be finalized")
	}
}

// This is the core of the whole design: the quorum cannot finalize a block the node does not have on its chain.
func TestQuorumCannotFinalizeForeignBlock(t *testing.T) {
	q := newQuorum(t, FinalitySigners, FinalityThreshold)
	f := NewFinalityState(q.set)
	// Our chain has a different block at height 100 than the quorum signs.
	ours := chainOf(map[uint64][32]byte{100: blockID(0xaa)})

	for i := 0; i < FinalitySigners; i++ {
		if _, err := f.Add(Sign(q.privs[i], uint8(i), 100, blockID(0xbb))); err != nil {
			t.Fatal(err)
		}
	}
	f.Update(200, ours)
	if _, _, ok := f.Finalized(); ok {
		t.Fatal("the quorum must not finalize a block we do not have — not even unanimously")
	}
}

// A double signature turns finality off for that height and produces proof. The chain keeps running.
func TestEquivocationDisablesFinalityForHeight(t *testing.T) {
	q := newQuorum(t, FinalitySigners, FinalityThreshold)
	f := NewFinalityState(q.set)
	ours := chainOf(map[uint64][32]byte{100: blockID(0xaa)})

	for i := 0; i < FinalityThreshold; i++ {
		f.Add(Sign(q.privs[i], uint8(i), 100, blockID(0xaa)))
	}
	ev, err := f.Add(Sign(q.privs[0], 0, 100, blockID(0xcc)))
	if err != ErrSignerConflict || ev == nil {
		t.Fatalf("a double signature should give proof and ErrSignerConflict, got %v %v", ev, err)
	}
	if ev.SignerIdx != 0 || ev.Height != 100 {
		t.Fatalf("the proof points at the wrong signer: %+v", ev)
	}
	f.Update(200, ours)
	if _, _, ok := f.Finalized(); ok {
		t.Fatal("after a double signature this height must not be finalized")
	}
}

func TestFinalityRejectsBadAttestations(t *testing.T) {
	q := newQuorum(t, FinalitySigners, FinalityThreshold)
	other := newQuorum(t, 1, 1)
	f := NewFinalityState(q.set)

	bad := Sign(other.privs[0], 0, 100, blockID(0xaa)) // a signature with a foreign key
	if _, err := f.Add(bad); err != ErrBadSignature {
		t.Fatalf("a foreign signature should give ErrBadSignature, it gave %v", err)
	}
	unknown := Sign(q.privs[0], uint8(FinalitySigners+3), 100, blockID(0xaa))
	if _, err := f.Add(unknown); err != ErrUnknownSigner {
		t.Fatalf("an unknown index should give ErrUnknownSigner, it gave %v", err)
	}
	// A signature for another height must not be usable (the domain and the height are in the message).
	moved := Sign(q.privs[0], 0, 100, blockID(0xaa))
	moved.Height = 101
	if _, err := f.Add(moved); err != ErrBadSignature {
		t.Fatalf("a moved signature should give ErrBadSignature, it gave %v", err)
	}
}

// A duplicate attestation from the same member counts once.
func TestDuplicateAttestationCountsOnce(t *testing.T) {
	q := newQuorum(t, FinalitySigners, FinalityThreshold)
	f := NewFinalityState(q.set)
	ours := chainOf(map[uint64][32]byte{100: blockID(0xaa)})
	for i := 0; i < FinalityThreshold+2; i++ {
		f.Add(Sign(q.privs[0], 0, 100, blockID(0xaa)))
	}
	f.Update(200, ours)
	if _, _, ok := f.Finalized(); ok {
		t.Fatal("one quorum member must not reach the threshold by repeating")
	}
}

// A reorg below a finalized block is refused, above it it is an ordinary thing.
func TestReorgGuard(t *testing.T) {
	q := newQuorum(t, FinalitySigners, FinalityThreshold)
	f := NewFinalityState(q.set)
	ours := chainOf(map[uint64][32]byte{100: blockID(0xaa)})
	for i := 0; i < FinalityThreshold; i++ {
		f.Add(Sign(q.privs[i], uint8(i), 100, blockID(0xaa)))
	}
	f.Update(200, ours)

	if err := f.CheckReorg(99); err != ErrFinalityRewrite {
		t.Fatalf("a reorg below a finalized block should be refused, got %v", err)
	}
	if err := f.CheckReorg(100); err != nil {
		t.Fatalf("a reorg from the finalized height upwards should pass, got %v", err)
	}
	if err := f.CheckReorg(150); err != nil {
		t.Fatalf("a shallow reorg should pass, got %v", err)
	}

	// Without finality nothing is blocked — the chain runs on plain PoW.
	empty := NewFinalityState(q.set)
	if err := empty.CheckReorg(1); err != nil {
		t.Fatalf("without attestations nothing may be blocked, got %v", err)
	}
}

// newQuorumForFuzz returns a quorum with valid keys but without a test context.
func newQuorumForFuzz() *SignerSet {
	set := &SignerSet{Threshold: FinalityThreshold}
	for i := 0; i < FinalitySigners; i++ {
		pub, _, err := ed25519.GenerateKey(nil)
		if err != nil {
			panic(err)
		}
		set.Keys = append(set.Keys, pub)
	}
	return set
}

// The range of a getattest request is the peer's. A range that ends at the last possible height, or
// covers every height there is, must cost no more than the attestations the node holds.
func TestInRangeDoesNotWalkTheRange(t *testing.T) {
	f := NewFinalityState(&SignerSet{Keys: RegTest.FinalitySigners, Threshold: 5})
	for _, h := range []uint64{7, 5, 6} {
		for s := range 2 {
			if _, err := f.Add(Sign(RegtestSignerKey(s), uint8(s), h, [32]byte{byte(h)})); err != nil {
				t.Fatal(err)
			}
		}
	}
	type span struct {
		from, to uint64
		want     []uint64
	}
	spans := []span{
		{math.MaxUint64, math.MaxUint64, nil},
		{0, math.MaxUint64, []uint64{5, 5, 6, 6, 7, 7}},
		{6, math.MaxUint64, []uint64{6, 6, 7, 7}},
		{6, 6, []uint64{6, 6}},
		{8, 5, nil},
	}
	for _, sp := range spans {
		done := make(chan []Attestation, 1)
		go func() { done <- f.InRange(sp.from, sp.to, 640) }()
		select {
		case got := <-done:
			var heights []uint64
			for _, a := range got {
				heights = append(heights, a.Height)
			}
			if !slices.Equal(heights, sp.want) {
				t.Fatalf("InRange(%d, %d) gave heights %v, want %v", sp.from, sp.to, heights, sp.want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("InRange(%d, %d) is still walking", sp.from, sp.to)
		}
	}
	if got := f.InRange(0, math.MaxUint64, 3); len(got) != 3 {
		t.Fatalf("the cap of 3 gave %d", len(got))
	}
}
