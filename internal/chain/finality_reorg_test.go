package chain

import (
	"testing"

	"powmachina/internal/mh"
)

// branchHeaders are `n` headers on top of `parent`, lanes taking turns; salt keeps two branches
// from the same parent apart.
func branchHeaders(parent *Header, n int, salt uint64) []*Header {
	out := make([]*Header, 0, n)
	prev := parent
	for range n {
		h := &Header{Version: 1, PrevBlock: prev.ID(), Bits: prev.Bits, Height: prev.Height + 1,
			Time: prev.Time + netSecs, Lane: mh.Lane((prev.Height + 1) % 2), Nonce: salt}
		out = append(out, h)
		prev = h
	}
	return out
}

// finalityChain is a header chain that respects `f`, with `n` blocks loaded.
func finalityChain(t *testing.T, f *FinalityState, n int) (*HeaderChain, []*Header) {
	t.Helper()
	g := RegtestGenesis()
	hc := NewHeaderChain(RegTest, g, f)
	hs := append([]*Header{g}, branchHeaders(g, n, 0)...)
	for _, h := range hs[1:] {
		if err := hc.LoadHeader(h); err != nil {
			t.Fatal(err)
		}
	}
	return hc, hs
}

// A reorg can land between the moment finality reads the chain and the moment it decides. The
// block it then finalizes must still be the one on the active branch — otherwise the node reports
// as final a block it no longer has, and refuses for good the branch that does have it.
func TestFinalityDoesNotLandOffTheActiveBranch(t *testing.T) {
	a := finalityKey(1)
	w := window{}.mined(1, 400, a)
	f := NewFinalityState()
	hc, hs := finalityChain(t, f, 150)
	other := branchHeaders(hs[40], 120, 7) // heavier: 160 blocks against 150
	const target = 60
	signAll(t, f, quorumsOf(w)(target), target, hs[target].ID(), a)

	reorged := false
	lookup := func(h uint64) ([32]byte, bool) {
		id, ok := hc.BlockIDAt(h)
		if !reorged {
			reorged = true
			for _, h := range other {
				if err := hc.LoadHeader(h); err != nil {
					t.Fatal(err)
				}
			}
		}
		return id, ok
	}
	f.UpdateOn(150, lookup, quorumsOf(w), hc.HoldActive)
	if got, id, ok := f.Finalized(); ok {
		if have, _ := hc.BlockIDAt(got); have != id {
			t.Fatalf("height %d is final with %x, the active branch has %x there", got, id[:4], have[:4])
		}
	}
}

// A finalized block whose body stalls is parked, and the tip falls below it. When the body comes,
// the way back to the finalized block is not a rewrite of it — it must be taken.
func TestAParkedFinalizedBlockComesBack(t *testing.T) {
	a := finalityKey(1)
	w := window{}.mined(1, 400, a)
	f := NewFinalityState()
	hc, hs := finalityChain(t, f, 200)
	const target = 100
	signAll(t, f, quorumsOf(w)(target), target, hs[target].ID(), a)
	f.Update(200, hc.BlockIDAt, quorumsOf(w))
	if got, _, ok := f.Finalized(); !ok || got != target {
		t.Fatalf("finality at %d (%v)", got, ok)
	}
	if _, changed, err := hc.Park(hs[95].ID()); err != nil || !changed {
		t.Fatalf("park: %v %v", changed, err)
	}
	if _, h, _ := hc.Tip(); h != 94 {
		t.Fatalf("the tip is at %d after the park", h)
	}
	// Headers keep coming on the finalized branch while it is parked.
	next := branchHeaders(hs[200], 1, 0)[0]
	if _, err := hc.AddHeader(next, acceptPoW{}); err != nil {
		t.Fatalf("a header on the finalized branch while it is parked: %v", err)
	}
	if moved, err := hc.Unpark(hs[95].ID()); err != nil || !moved {
		t.Fatalf("the body came, the branch with the finalized block was not taken back: %v %v", moved, err)
	}
	if _, _, tip := hc.Tip(); tip != next.ID() {
		t.Fatal("the tip is not back on the finalized branch")
	}
	// A branch that does not hold the finalized block is still refused.
	if _, err := hc.AddHeader(branchHeaders(hs[90], 1, 9)[0], acceptPoW{}); err == nil {
		t.Fatal("a branch below the finalized block was taken")
	}
}
