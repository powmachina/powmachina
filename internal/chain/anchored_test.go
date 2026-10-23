package chain

import (
	"errors"
	"testing"
	"time"

	"powmachina/internal/mh"
)

type anyPoW struct{}

func (anyPoW) VerifyPoW(*Header) (bool, error) { return true, nil }

// cheapBranch files `count` headers at the retarget's difficulty on top of the genesis: timestamps
// far enough apart keep it at or near the floor, which is what makes such a history cheap to make.
func cheapBranch(t *testing.T, hc *HeaderChain, g *Header, count int, salt uint64) (last [32]byte, firstErr error) {
	t.Helper()
	prev, tm := g.ID(), g.Time
	for i := 1; i <= count; i++ {
		tm += 240
		lane := mh.LaneG
		if i%3 == 0 {
			lane = mh.LaneC
		}
		hc.mu.RLock()
		bits, err := hc.nextBitsLocked(hc.byID[prev], lane)
		hc.mu.RUnlock()
		if err != nil {
			t.Fatal(err)
		}
		h := &Header{Version: 1, PrevBlock: prev, Bits: bits, Height: uint32(i), Time: tm, Lane: lane,
			Nonce: salt<<32 | uint64(i)}
		if _, err := hc.AddHeader(h, anyPoW{}); err != nil {
			return prev, err
		}
		prev = h.ID()
	}
	return prev, nil
}

// A node on its first download takes headers from whoever answers first. ReorgLimit + 1 cheap
// headers used to be enough to hold it: the fork depth limit, measured from that tip, refused the
// real chain at height 1, for good. Below its checkpoints a node defends nothing and follows the
// most work.
func TestAChainBelowItsCheckpointsCanBeLeft(t *testing.T) {
	p := TestNet
	p.Checkpoints = []Checkpoint{{Height: 2 * ReorgLimit, ID: [32]byte{0xAA}}}
	p.MinChainWork = "ffffffffff"
	g := TestnetGenesis()
	hc := NewHeaderChain(p, g, nil)
	hc.now = func() time.Time { return time.Unix(int64(g.Time)+10_000_000, 0) }

	fake, err := cheapBranch(t, hc, g, int(ReorgLimit)+1, 1)
	if err != nil {
		t.Fatalf("setup: the first branch was refused: %v", err)
	}
	if _, _, tip := hc.Tip(); tip != fake {
		t.Fatal("setup: the first branch is not the tip")
	}
	if hc.Anchored() {
		t.Fatal("a chain below the checkpoint counts as anchored")
	}

	// Another branch from the genesis, longer: every header is taken, and with more work it wins.
	other, err := cheapBranch(t, hc, g, int(ReorgLimit)+50, 2)
	if err != nil {
		t.Fatalf("a branch that forks at the genesis was refused: %v", err)
	}
	if _, _, tip := hc.Tip(); tip != other {
		t.Fatal("the branch with more work did not take the tip")
	}
}

// With nothing to measure a chain against, the limit holds as it always did.
func TestAnAnchoredChainKeepsItsForkDepthLimit(t *testing.T) {
	p := TestNet
	p.Checkpoints, p.MinChainWork = nil, ""
	g := TestnetGenesis()
	hc := NewHeaderChain(p, g, nil)
	hc.now = func() time.Time { return time.Unix(int64(g.Time)+10_000_000, 0) }
	if _, err := cheapBranch(t, hc, g, int(ReorgLimit)+1, 1); err != nil {
		t.Fatal(err)
	}
	if !hc.Anchored() {
		t.Fatal("a chain on a network without checkpoints is not anchored")
	}
	if _, err := cheapBranch(t, hc, g, 1, 2); !errors.Is(err, ErrForkTooDeep) {
		t.Fatalf("a fork %d blocks deep: %v, want ErrForkTooDeep", ReorgLimit+1, err)
	}
}
