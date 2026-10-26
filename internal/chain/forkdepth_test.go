package chain

import (
	"errors"
	"testing"

	"powmachina/internal/mh"
)

type acceptPoW struct{}

func (acceptPoW) VerifyPoW(*Header) (bool, error) { return true, nil }

// mainChain builds an active branch of `n` blocks alternating C/G through LoadHeader (without the
// checks) and returns it together with the IDs by height.
func mainChain(t testing.TB, params NetParams, n int) (*HeaderChain, []*Header) {
	t.Helper()
	g := RegtestGenesis()
	hc := NewHeaderChain(params, g, nil)
	hs := []*Header{g}
	prev := g.ID()
	for i := 1; i <= n; i++ {
		lane := mh.LaneC
		if i%2 == 0 {
			lane = mh.LaneG
		}
		h := &Header{Version: 1, PrevBlock: prev, Bits: g.Bits, Height: uint32(i),
			Time: g.Time + uint32(i)*netSecs, Lane: lane}
		if err := hc.LoadHeader(h); err != nil {
			t.Fatalf("block %d: %v", i, err)
		}
		hs = append(hs, h)
		prev = h.ID()
	}
	return hc, hs
}

// sideHeader builds a header that forks from height `at` and passes the cheap checks.
func sideHeader(t *testing.T, hc *HeaderChain, parent *Header) *Header {
	t.Helper()
	hc.mu.RLock()
	pn := hc.byID[parent.ID()]
	lane := mh.LaneC
	if parent.Lane == mh.LaneC {
		lane = mh.LaneG
	}
	bits, err := hc.nextBitsLocked(pn, lane)
	hc.mu.RUnlock()
	if err != nil {
		t.Fatal(err)
	}
	return &Header{Version: 1, PrevBlock: parent.ID(), Bits: bits, Height: parent.Height + 1,
		Time: parent.Time + 60, Lane: lane, Nonce: 0xdead}
}

// A branch forking shallower than ReorgLimit is stored — it could win one day.
func TestSideBranchWithinReorgLimitIsKept(t *testing.T) {
	n := int(ReorgLimit) + 200
	hc, hs := mainChain(t, RegTest, n)
	h := sideHeader(t, hc, hs[n-10])
	if _, err := hc.AddHeader(h, acceptPoW{}); err != nil {
		t.Fatalf("a branch 10 blocks below the tip was refused: %v", err)
	}
}

// A branch forking deeper than ReorgLimit is refused right away — the node would never switch to
// it, and storing it would let an attacker fill memory with cheap headers from early heights.
func TestForkDeeperThanReorgLimitIsRejected(t *testing.T) {
	n := int(ReorgLimit) + 200
	hc, hs := mainChain(t, RegTest, n)
	h := sideHeader(t, hc, hs[100])
	if _, err := hc.AddHeader(h, acceptPoW{}); !errors.Is(err, ErrForkTooDeep) {
		t.Fatalf("a branch %d blocks below the tip: %v, expected ErrForkTooDeep", n-100, err)
	}
}

// The depth rule is checked before PoW — a refusal must not cost a PoW computation.
func TestDeepForkRejectedBeforePoW(t *testing.T) {
	hc, hs := mainChain(t, RegTest, int(ReorgLimit)+200)
	h := sideHeader(t, hc, hs[50])
	var called bool
	check := powFunc(func(*Header) (bool, error) { called = true; return true, nil })
	_, _ = hc.AddHeader(h, check)
	if called {
		t.Fatal("PoW was computed for a deep branch — an attacker would keep us busy with it")
	}
}

type powFunc func(*Header) (bool, error)

func (f powFunc) VerifyPoW(h *Header) (bool, error) { return f(h) }

// A header at a checkpoint height has to have exactly its ID.
func TestCheckpointMismatchIsRejected(t *testing.T) {
	params := RegTest
	params.Checkpoints = []Checkpoint{{Height: 6, ID: [32]byte{0xaa}}}
	hc, hs := mainChain(t, params, 5)
	h := sideHeader(t, hc, hs[5]) // height 6, a different ID
	if _, err := hc.AddHeader(h, acceptPoW{}); !errors.Is(err, ErrCheckpoint) {
		t.Fatalf("a header conflicting with the checkpoint: %v, expected ErrCheckpoint", err)
	}
}

// The search for the fork point must not walk the whole chain — a naive one takes as many steps
// as the chain has blocks for every header from a side branch.
func TestForkPointSearchIsBounded(t *testing.T) {
	hc, hs := mainChain(t, RegTest, 3_000)
	hc.mu.RLock()
	defer hc.mu.RUnlock()
	parent := hc.byID[hs[2_990].ID()]
	if fp := hc.forkPointBoundedLocked(parent, ReorgLimit); fp == nil || fp.height != 2_990 {
		t.Fatalf("the fork point is %v, expected height 2 990", fp)
	}
	deep := hc.byID[hs[100].ID()]
	if fp := hc.forkPointBoundedLocked(deep, ReorgLimit); fp != nil {
		t.Fatalf("a deep fork point was found (height %d) — it should have been refused", fp.height)
	}
}
