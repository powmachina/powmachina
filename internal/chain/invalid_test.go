package chain

import (
	"errors"
	"math/big"
	"testing"
)

// fakeChain builds a header chain directly, without PoW — we are testing branch selection, not validation.
type fakeChain struct{ hc *HeaderChain }

func newFakeChain(t *testing.T) *fakeChain {
	t.Helper()
	hc := NewHeaderChain(RegTest, RegtestGenesis(), nil)
	return &fakeChain{hc: hc}
}

// add appends a node under a given parent with a given work. It returns its id.
func (f *fakeChain) add(t *testing.T, parent [32]byte, tag byte, work int64) [32]byte {
	t.Helper()
	p, ok := f.hc.byID[parent]
	if !ok {
		t.Fatalf("the parent %x is not known", parent[:4])
	}
	var id [32]byte
	id[0], id[1] = tag, byte(p.height+1)
	hdr := &Header{Version: 1, PrevBlock: parent, Height: uint32(p.height + 1)}
	n := &node{header: hdr, id: id, work: new(big.Int).Add(p.work, big.NewInt(work)),
		height: p.height + 1}
	f.hc.byID[id] = n
	if n.work.Cmp(f.hc.tip.work) > 0 {
		f.hc.tip = n
	}
	return id
}

// An invalidation has to switch to the heaviest branch with no invalid ancestor — and drag the
// whole offspring of the invalid block with it, no matter how heavy it is.
func TestInvalidateSwitchesToHeaviestValidBranch(t *testing.T) {
	f := newFakeChain(t)
	g := f.hc.genesis

	// A valid branch: g → a1 → a2 (work 20)
	a1 := f.add(t, g, 'a', 10)
	a2 := f.add(t, a1, 'a', 10)
	// A heavier branch: g → b1 → b2 → b3 (work 90) — the tip moves onto it
	b1 := f.add(t, g, 'b', 30)
	b2 := f.add(t, b1, 'b', 30)
	b3 := f.add(t, b2, 'b', 30)
	if _, _, tip := f.hc.Tip(); tip != b3 {
		t.Fatalf("before the invalidation the tip should be b3, it is %x", tip[:4])
	}

	// b2 is invalid → b3 falls too, even though it is the heaviest.
	newTip, changed, err := f.hc.Invalidate(b2)
	if err != nil || !changed {
		t.Fatalf("the invalidation: changed=%v, err=%v", changed, err)
	}
	if newTip != b1 {
		t.Fatalf("expected a switch to b1 (work 30), got %x", newTip[:4])
	}
	if !f.hc.IsInvalid(b3) {
		t.Fatal("a child of an invalid block has to be invalid too")
	}
	if f.hc.IsInvalid(a2) || f.hc.IsInvalid(b1) {
		t.Fatal("the valid branch was marked invalid")
	}

	// Once b1 falls too, branch a wins (work 20).
	if _, _, err := f.hc.Invalidate(b1); err != nil {
		t.Fatal(err)
	}
	if _, _, tip := f.hc.Tip(); tip != a2 {
		t.Fatalf("expected a switch to a2, got %x", tip[:4])
	}
}

// The genesis is the root of trust from the binary — it cannot be invalidated.
func TestInvalidateRefusesGenesis(t *testing.T) {
	f := newFakeChain(t)
	if _, _, err := f.hc.Invalidate(f.hc.genesis); err == nil {
		t.Fatal("the genesis should not have been markable as invalid")
	}
}

// With equal work every node has to pick the same tip, otherwise the network falls apart into two
// halves with different opinions about what the tip is.
func TestInvalidateTieBreakIsDeterministic(t *testing.T) {
	first := [32]byte{}
	for i := 0; i < 5; i++ {
		f := newFakeChain(t)
		g := f.hc.genesis
		bad := f.add(t, g, 'z', 100)
		// two equally heavy valid branches
		f.add(t, g, 'c', 10)
		f.add(t, g, 'd', 10)
		tip, _, err := f.hc.Invalidate(bad)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = tip
			continue
		}
		if tip != first {
			t.Fatalf("the choice is not deterministic: %x vs %x", tip[:4], first[:4])
		}
	}
}

// A header that grows out of an invalid block has to be refused **before** the PoW check —
// otherwise repeating the attack is free.
func TestAddHeaderRejectsInvalidAncestorBeforePoW(t *testing.T) {
	f := newFakeChain(t)
	bad := f.add(t, f.hc.genesis, 'z', 100)
	if _, _, err := f.hc.Invalidate(bad); err != nil {
		t.Fatal(err)
	}
	child := &Header{Version: 1, PrevBlock: bad, Height: 2}
	// pow is nil: if the computation reached it, the test would crash on a nil pointer.
	if _, err := f.hc.AddHeader(child, nil); !errors.Is(err, ErrInvalidAncestor) {
		t.Fatalf("expected ErrInvalidAncestor, got %v", err)
	}
}
