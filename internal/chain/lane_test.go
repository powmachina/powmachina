package chain

import (
	"errors"
	"testing"
	"time"

	"powmachina/internal/mh"
)

func TestLaneRotationRule(t *testing.T) {
	// Two of the same in a row are fine, a third one is not.
	if !LaneAllowed([]mh.Lane{mh.LaneC}, mh.LaneC) {
		t.Fatal("one predecessor of the same lane is fine")
	}
	if LaneAllowed([]mh.Lane{mh.LaneC, mh.LaneC}, mh.LaneC) {
		t.Fatal("a third block of the same lane in a row has to be invalid")
	}
	if !LaneAllowed([]mh.Lane{mh.LaneC, mh.LaneC}, mh.LaneG) {
		t.Fatal("another lane after two of the same is fine")
	}
	if !LaneAllowed([]mh.Lane{mh.LaneC, mh.LaneG}, mh.LaneC) {
		t.Fatal("a broken run does not count")
	}
	// The start of the chain.
	if !LaneAllowed(nil, mh.LaneC) {
		t.Fatal("an empty history must not block anything")
	}
}

// When the network stalls the rule is waived — otherwise one lane dying out would stop the chain.
func TestLaneRotationRelief(t *testing.T) {
	recent := []mh.Lane{mh.LaneC, mh.LaneC}
	parent := time.Unix(1_700_000_000, 0)

	block := parent.Add(LaneRotationRelief - time.Second)
	if LaneAllowedAt(recent, mh.LaneC, block, parent) {
		t.Fatal("before the relief elapses the rule has to apply")
	}
	block = parent.Add(LaneRotationRelief + time.Second)
	if !LaneAllowedAt(recent, mh.LaneC, block, parent) {
		t.Fatal("after the relief elapses the rule has to be waived")
	}
	// An ordinary gap on a healthy chain must NOT trigger the relief.
	if LaneAllowedAt(recent, mh.LaneC, parent.Add(3*time.Minute), parent) {
		t.Fatal("three minutes between blocks is not a stalled network")
	}
}

// A pool needs to know which lanes it may hand out a template for.
func TestAllowedLanes(t *testing.T) {
	parent := time.Unix(1_700_000_000, 0)
	now := parent.Add(time.Minute)

	all := AllowedLanes([]mh.Lane{mh.LaneG, mh.LaneC}, now, parent)
	if len(all) != len(mh.Lanes) {
		t.Fatalf("without a run both lanes should be allowed, got %v", all)
	}

	// After two blocks of the same lane, with two lanes only one option is left — and that is
	// exactly what forces an attacker to have both domains.
	blocked := AllowedLanes([]mh.Lane{mh.LaneC, mh.LaneC}, now, parent)
	if len(blocked) != 1 || blocked[0] != mh.LaneG {
		t.Fatalf("after two C only G should be left, got %v", blocked)
	}
	blocked = AllowedLanes([]mh.Lane{mh.LaneG, mh.LaneG}, now, parent)
	if len(blocked) != 1 || blocked[0] != mh.LaneC {
		t.Fatalf("after two G only C should be left, got %v", blocked)
	}

	// Liveness: when the network stands still longer than the relief, the rule does not apply and
	// the chain does not stall even when one domain dies out entirely.
	late := parent.Add(LaneRotationRelief + time.Second)
	if got := AllowedLanes([]mh.Lane{mh.LaneC, mh.LaneC}, late, parent); len(got) != len(mh.Lanes) {
		t.Fatalf("after the relief both lanes should be allowed, got %v", got)
	}
}

// The relief may release **one** block, not a burst. After it the gap is measured from that block
// and the rotation rule applies again — otherwise one lane would pass in a run after every stall.
//
// Measured against the median time past instead, the relief lets six blocks through while the
// median catches up — and since the MTP trails the tip by ~6 blocks, the relief applies all the
// time: one lane can run dozens of blocks in a row.
func TestLaneReliefReleasesOneBlockOnly(t *testing.T) {
	g := RegtestGenesis()
	hc := NewHeaderChain(RegTest, g, nil)

	// A chain ending with two lane G blocks; another G therefore needs the relief.
	stalled := uint32(time.Now().Add(-time.Hour).Unix())
	const pre = 40
	prev := g.ID()
	for i := 1; i <= pre; i++ {
		lane := mh.LaneC
		if i%2 == 0 || i == pre {
			lane = mh.LaneG
		}
		h := &Header{Version: 1, PrevBlock: prev, Bits: g.Bits, Height: uint32(i), Lane: lane,
			Time: stalled - uint32(pre-i)*120}
		if err := hc.LoadHeader(h); err != nil {
			t.Fatalf("block %d: %v", i, err)
		}
		prev = h.ID()
	}

	// The network starts moving again and only lane G reports in.
	fresh := uint32(time.Now().Add(-time.Minute).Unix())
	var burst int
	for i := range 6 {
		h := laneHeader(t, hc, mh.LaneG, fresh+uint32(i))
		ok, err := hc.AddHeader(h, acceptPoW{})
		if errors.Is(err, ErrLaneNotAllowed) {
			break
		}
		if err != nil || !ok {
			t.Fatalf("block %d: ok=%v, %v", i, ok, err)
		}
		burst++
	}
	if burst != 1 {
		t.Fatalf("the relief let %d blocks of one lane through, expected 1", burst)
	}
	// And the other lane goes right away — the chain does not stand still.
	if _, err := hc.AddHeader(laneHeader(t, hc, mh.LaneC, fresh+1), acceptPoW{}); err != nil {
		t.Fatalf("lane C after the relief: %v", err)
	}
}

// On a healthy chain the rule must NOT be waived. This is the case that was broken in practice:
// blocks arrive a few minutes apart — completely ordinary with a 120 s target — and yet one lane
// mined without a break.
func TestLaneRuleHoldsOnHealthyChain(t *testing.T) {
	g := RegtestGenesis()
	hc := NewHeaderChain(RegTest, g, nil)

	start := uint32(time.Now().Add(-2 * time.Hour).Unix())
	prev := g.ID()
	// 20 blocks alternating lanes with a 180 s gap (above the target, below the relief).
	var t0 uint32 = start
	for i := 1; i <= 20; i++ {
		lane := mh.LaneC
		if i%2 == 0 {
			lane = mh.LaneG
		}
		h := &Header{Version: 1, PrevBlock: prev, Bits: g.Bits, Height: uint32(i), Lane: lane,
			Time: t0}
		if err := hc.LoadHeader(h); err != nil {
			t.Fatalf("block %d: %v", i, err)
		}
		prev = h.ID()
		t0 += 180
	}

	// Two lane C blocks in a row pass, the third does not — even though 180 s have passed since the
	// parent and the median time past is long behind us.
	for i := range 2 {
		h := laneHeader(t, hc, mh.LaneC, t0+uint32(i)*180)
		if _, err := hc.AddHeader(h, acceptPoW{}); err != nil {
			t.Fatalf("block C %d: %v", i+1, err)
		}
	}
	h := laneHeader(t, hc, mh.LaneC, t0+2*180)
	if _, err := hc.AddHeader(h, acceptPoW{}); !errors.Is(err, ErrLaneNotAllowed) {
		t.Fatalf("a third lane C block passed on a healthy chain (%v) — the rotation rule does nothing", err)
	}
}

// laneHeader assembles a header on the tip with the bits from the given lane's retarget.
func laneHeader(t *testing.T, hc *HeaderChain, lane mh.Lane, at uint32) *Header {
	t.Helper()
	hc.mu.RLock()
	tip := hc.tip
	bits, err := hc.nextBitsLocked(tip, lane)
	hc.mu.RUnlock()
	if err != nil {
		t.Fatal(err)
	}
	return &Header{Version: 1, PrevBlock: tip.id, Bits: bits, Height: uint32(tip.height) + 1,
		Lane: lane, Time: at, Nonce: 0xbeef}
}

// The genesis must not decide which lane may mine the network's first blocks — its lane is a
// constant from the binary. Without this exception a CPU miner on a fresh testnet would mine one
// block and then wait for a GPU to show up.
func TestGenesisLaneDoesNotCountToRotation(t *testing.T) {
	g := RegtestGenesis() // lane C
	if g.Lane != mh.LaneC {
		t.Fatalf("test assumption: the genesis is lane C, it is %s", g.Lane)
	}
	hc := NewHeaderChain(RegTest, g, nil)

	// The first block in the same lane as the genesis has to pass…
	h1 := &Header{Version: 1, PrevBlock: g.ID(), Bits: g.Bits, Height: 1,
		Time: g.Time + 60, Lane: mh.LaneC, Nonce: 1}
	if ok, err := hc.AddHeader(h1, acceptPoW{}); err != nil || !ok {
		t.Fatalf("block 1 in the genesis lane: ok=%v, %v", ok, err)
	}
	// …and so does the second, because the genesis does not count.
	h2 := &Header{Version: 1, PrevBlock: h1.ID(), Bits: g.Bits, Height: 2,
		Time: h1.Time + 60, Lane: mh.LaneC, Nonce: 2}
	if ok, err := hc.AddHeader(h2, acceptPoW{}); err != nil || !ok {
		t.Fatalf("block 2 in the genesis lane: ok=%v, %v", ok, err)
	}
	// The third no longer does — the rule applies normally.
	h3 := &Header{Version: 1, PrevBlock: h2.ID(), Bits: g.Bits, Height: 3,
		Time: h2.Time + 60, Lane: mh.LaneC, Nonce: 3}
	if _, err := hc.AddHeader(h3, acceptPoW{}); !errors.Is(err, ErrLaneNotAllowed) {
		t.Fatalf("a third block of the same lane: %v, expected ErrLaneNotAllowed", err)
	}
}

// The lanes a node offers must be the lanes a block built this second is valid for. A header's time
// is whole seconds, so a clock reading of 900.4 s after the parent makes a header 900 s after it —
// and that is not past the relief yet.
func TestAllowedLanesAgreeWithTheHeaderSecond(t *testing.T) {
	parent := time.Unix(1_800_000_000, 0)
	recent := []mh.Lane{mh.LaneG, mh.LaneG}
	has := func(lanes []mh.Lane, l mh.Lane) bool {
		for _, x := range lanes {
			if x == l {
				return true
			}
		}
		return false
	}
	for _, ms := range []int64{899_900, 900_000, 900_400, 900_999, 901_000, 901_500} {
		now := parent.Add(time.Duration(ms) * time.Millisecond)
		header := time.Unix(now.Unix(), 0) // what work.Build writes into the block
		offered := has(AllowedLanes(recent, now, parent), mh.LaneG)
		valid := LaneAllowedAt(recent, mh.LaneG, header, parent)
		if offered != valid {
			t.Errorf("%d ms after the parent: lane G offered = %v, but a block built now is valid = %v", ms, offered, valid)
		}
	}
}
