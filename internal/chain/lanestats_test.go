package chain

import (
	"testing"

	"powmachina/internal/mh"
)

// buildLanes builds a header chain with a given sequence of lanes.
//
// It goes through LoadHeader, that is, the restore path, which does not recompute PoW — for the
// lane statistics what matters is which lane mined a block and when, not whether the nonce fits.
func buildLanes(t *testing.T, lanes []mh.Lane, spacing uint32) *HeaderChain {
	t.Helper()
	g := RegtestGenesis()
	hc := NewHeaderChain(RegTest, g, nil)

	prev := g.ID()
	tm := g.Time
	for i, lane := range lanes {
		tm += spacing
		h := &Header{
			Version:   1,
			PrevBlock: prev,
			Bits:      g.Bits,
			Height:    uint32(i + 1),
			Time:      tm,
			Lane:      lane,
		}
		if err := hc.LoadHeader(h); err != nil {
			t.Fatalf("block %d (%s): %v", i+1, lane, err)
		}
		prev = h.ID()
	}
	return hc
}

// The share of blocks has to match what was really mined — that is the metric that shows whether
// CPU/GPU parity works.
func TestLaneStatsShareMatchesMinedBlocks(t *testing.T) {
	// 20 blocks, alternating C and G — the share should be exactly half and half.
	lanes := make([]mh.Lane, 0, 20)
	for i := range 20 {
		if i%2 == 0 {
			lanes = append(lanes, mh.LaneC)
		} else {
			lanes = append(lanes, mh.LaneG)
		}
	}
	st := buildLanes(t, lanes, 120).LaneStats()

	if st.TipHeight != 20 {
		t.Fatalf("height %d, expected 20", st.TipHeight)
	}
	if len(st.Lanes) != len(mh.Lanes) {
		t.Fatalf("the statistics have %d lanes, consensus has %d", len(st.Lanes), len(mh.Lanes))
	}
	// In the window of 10 both have five blocks each.
	for _, l := range st.Lanes {
		if l.BlocksInWindow[0] != 5 {
			t.Fatalf("lane %s has %d blocks in the window of 10, expected 5", l.Lane, l.BlocksInWindow[0])
		}
	}
	// The sum across the lanes must not exceed the window size — otherwise a block is counted twice.
	for wi, w := range st.Windows {
		var sum int
		for _, l := range st.Lanes {
			sum += l.BlocksInWindow[wi]
		}
		if sum > w {
			t.Fatalf("the window of %d holds %d blocks in total — one of them is counted more than once", w, sum)
		}
	}
}

// A lane that is not mining has to have a zero share — and not disappear from the list.
// The explorer has to be able to show that one side of the network is collapsing.
func TestLaneStatsShowsStarvedLane(t *testing.T) {
	// All C — G has not a single block. (The rotation rule is not enforced here, LoadHeader is the
	// restore path; the test is about the statistics, not about validation.)
	lanes := make([]mh.Lane, 12)
	for i := range lanes {
		lanes[i] = mh.LaneC
	}
	st := buildLanes(t, lanes, 120).LaneStats()

	var seenG bool
	for _, l := range st.Lanes {
		if l.Lane != mh.LaneG {
			continue
		}
		seenG = true
		if l.BlocksInWindow[0] != 0 {
			t.Fatalf("G mined nothing but reports %d blocks", l.BlocksInWindow[0])
		}
		if l.LastHeight != 0 {
			t.Fatalf("G has no block but reports last height %d", l.LastHeight)
		}
		if l.HashrateEst != nil && l.HashrateEst.Sign() != 0 {
			t.Fatalf("G mined nothing but reports a hashrate of %s", l.HashrateEst)
		}
	}
	if !seenG {
		t.Fatal("G disappeared from the statistics — the explorer could not show that a lane is collapsing")
	}
}

// A run of the same lane at the tip is a consensus rule, and when it is broken it has to be visible
// in the explorer.
func TestLaneStatsCountsRun(t *testing.T) {
	st := buildLanes(t, []mh.Lane{mh.LaneC, mh.LaneG, mh.LaneC, mh.LaneC}, 120).LaneStats()
	if st.RunLane != mh.LaneC {
		t.Fatalf("at the tip there is %s, expected C", st.RunLane)
	}
	if st.RunLength != 2 {
		t.Fatalf("the run has %d blocks, expected 2", st.RunLength)
	}
	if st.RunLength > MaxSameLaneRun {
		t.Fatalf("the run of %d exceeds MaxSameLaneRun %d", st.RunLength, MaxSameLaneRun)
	}

	// One block at the tip = a run of one.
	st2 := buildLanes(t, []mh.Lane{mh.LaneC, mh.LaneC, mh.LaneG}, 120).LaneStats()
	if st2.RunLane != mh.LaneG || st2.RunLength != 1 {
		t.Fatalf("the run is %s/%d, expected G/1", st2.RunLane, st2.RunLength)
	}
}

// The average spacing has to say whether a lane is keeping up with its target. With two lanes
// alternating at a network spacing of 120 s, one lane's spacing is 240 s — exactly LaneBlockTime.
func TestLaneStatsSpacingMatchesTarget(t *testing.T) {
	lanes := make([]mh.Lane, 0, 40)
	for i := range 40 {
		if i%2 == 0 {
			lanes = append(lanes, mh.LaneC)
		} else {
			lanes = append(lanes, mh.LaneG)
		}
	}
	st := buildLanes(t, lanes, 120).LaneStats()

	want := LaneBlockTime.Seconds()
	for _, l := range st.Lanes {
		if l.AvgSpacingSec < want*0.9 || l.AvgSpacingSec > want*1.1 {
			t.Fatalf("lane %s has a spacing of %.0f s, the target is %.0f s", l.Lane, l.AvgSpacingSec, want)
		}
	}
}

// This is exactly the case that failed in a live run on regtest and that the tests above missed,
// because they build an evenly spaced chain from the genesis time.
//
// The genesis has its time written into the binary; when the chain gets going months later, the gap
// between the genesis and the first block is arbitrarily large. A spacing computed from the window
// length would then come out in the millions of seconds against a target of 240 s, and the hashrate
// would drop to zero after the division.
func TestLaneStatsIgnoresGenesisTimestamp(t *testing.T) {
	g := RegtestGenesis()
	hc := NewHeaderChain(RegTest, g, nil)

	// The first block appears a year after the genesis, the rest every 120 s as on a live network.
	const yearSec = 365 * 24 * 3600
	prev := g.ID()
	tm := g.Time + yearSec
	for i := range 24 {
		lane := mh.LaneC
		if i%2 == 1 {
			lane = mh.LaneG
		}
		// The difficulty here is deliberately not the regtest one: with a trivial target (16
		// attempts) the work per block is smaller than the spacing in seconds and the hashrate
		// estimate comes out as 0 H/s. The test is about spacings and shares, not about how cheaply
		// one can mine on regtest.
		h := &Header{
			Version: 1, PrevBlock: prev, Bits: MaxTargetBits,
			Height: uint32(i + 1), Time: tm, Lane: lane,
		}
		if err := hc.LoadHeader(h); err != nil {
			t.Fatalf("block %d: %v", i+1, err)
		}
		prev = h.ID()
		tm += 120
	}

	st := hc.LaneStats()
	want := LaneBlockTime.Seconds()
	for _, l := range st.Lanes {
		if l.BlocksInWindow[0] == 0 {
			t.Fatalf("lane %s has not a single block in the window of 10", l.Lane)
		}
		if l.AvgSpacingSec < want*0.9 || l.AvgSpacingSec > want*1.1 {
			t.Fatalf("lane %s has a spacing of %.0f s, the target is %.0f s — the genesis time got into the average",
				l.Lane, l.AvgSpacingSec, want)
		}
		if l.HashrateEst == nil || l.HashrateEst.Sign() <= 0 {
			t.Fatalf("lane %s is mining, but the hashrate came out as %v", l.Lane, l.HashrateEst)
		}
	}
}

// The share has to be computed against how many blocks are really in the window. A chain with 24
// blocks does not have a window of 120, and half the network in it is not 14 %.
func TestLaneStatsWindowFillTracksShortChain(t *testing.T) {
	lanes := make([]mh.Lane, 0, 24)
	for i := range 24 {
		if i%2 == 0 {
			lanes = append(lanes, mh.LaneC)
		} else {
			lanes = append(lanes, mh.LaneG)
		}
	}
	st := buildLanes(t, lanes, 120).LaneStats()

	for wi, w := range st.Windows {
		fill := st.WindowFill[wi]
		if fill > w {
			t.Fatalf("the window of %d reports %d blocks — more than its size", w, fill)
		}
		if fill > 24 {
			t.Fatalf("the window of %d reports %d blocks, the chain has 24 (without the genesis)", w, fill)
		}
		var sum int
		for _, l := range st.Lanes {
			sum += l.BlocksInWindow[wi]
		}
		if sum != fill {
			t.Fatalf("the window of %d holds %d blocks, the lanes report %d in total", w, fill, sum)
		}
	}
	// The window of 120 is larger than the chain, so it holds all 24 blocks and the share is half and half.
	last := len(st.Windows) - 1
	if st.WindowFill[last] != 24 {
		t.Fatalf("the largest window holds %d blocks, expected 24", st.WindowFill[last])
	}
	for _, l := range st.Lanes {
		if l.BlocksInWindow[last] != 12 {
			t.Fatalf("lane %s has %d blocks in the window of %d, expected 12",
				l.Lane, st.Windows[last], l.BlocksInWindow[last])
		}
	}
}
