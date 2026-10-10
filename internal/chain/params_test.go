package chain

import (
	"testing"
	"time"

	"powmachina/internal/mh"
)

// Parameters that are **derived** from other parameters. A test on their value is not enough — it
// passes even when what they are derived from has changed in the meantime. So the relationship is
// what gets checked here.
//
// Why this is here: a LaneBlockTime of 360 s is right for three lanes. With two lanes it would
// give a block every 180 s instead of 120 s and the whole emission axis would stretch 1.5× —
// halving from 4 years to 6. Every LWMA test would still pass, because they are expressed
// relative to LaneBlockTime.

// The network's target block time has to follow from the number of lanes. When a lane is added or
// removed, LaneBlockTime has to be recomputed.
func TestLaneBlockTimeMatchesLaneCount(t *testing.T) {
	lanes := time.Duration(len(mh.Lanes))
	if got := LaneBlockTime / lanes; got != BlockTime {
		t.Fatalf("%d lanes × %v = %v network block, expected %v — recompute LaneBlockTime",
			len(mh.Lanes), LaneBlockTime, got, BlockTime)
	}
	if LaneBlockTime%lanes != 0 {
		t.Fatalf("LaneBlockTime %v does not divide by the number of lanes (%d) without a remainder",
			LaneBlockTime, len(mh.Lanes))
	}
}

// The time promises in the comments on the emission constants are computed from BlockTime. If
// BlockTime changed, they would stop holding — and that is exactly what nobody notices.
func TestEmissionTimelineMatchesBlockTime(t *testing.T) {
	const year = 365 * 24 * time.Hour
	cases := []struct {
		name   string
		blocks uint64
		want   time.Duration
		tol    time.Duration
	}{
		{"halving ≈ 4 years", HalvingInterval, 4 * year, 60 * 24 * time.Hour},
		{"end of the dev fee ≈ 2 years", DevFeeEndHeight, 2 * year, 30 * 24 * time.Hour},
		{"slow start ≈ 7 days", SlowStartBlocks, 7 * 24 * time.Hour, 12 * time.Hour},
		{"DAG epoch ≈ 6.9 days", EpochBlocks, 166 * time.Hour, 6 * time.Hour},
		{"reorg limit ≈ 16.7 h", ReorgLimit, 16*time.Hour + 40*time.Minute, time.Hour},
	}
	for _, c := range cases {
		got := time.Duration(c.blocks) * BlockTime
		if d := got - c.want; d < -c.tol || d > c.tol {
			t.Errorf("%s: %d blocks × %v = %v, expected ≈ %v",
				c.name, c.blocks, BlockTime, got.Round(time.Hour), c.want)
		}
	}
}

// The rotation rule only makes sense with at least two lanes and must not allow a whole run of one
// lane. With three or more lanes it stops forcing an attacker into the other one (ALGO §7.7).
func TestLaneRotationRuleFitsLaneCount(t *testing.T) {
	if len(mh.Lanes) != 2 {
		t.Fatalf("consensus is built on two lanes, mh.Lanes has %d — "+
			"revisit the rotation rule and the chainwork normalization", len(mh.Lanes))
	}
	if MaxSameLaneRun >= len(mh.Lanes)+1 {
		t.Fatalf("MaxSameLaneRun %d with %d lanes does not force alternation",
			MaxSameLaneRun, len(mh.Lanes))
	}
	// The relief has to kick in only when the network really stands still, not on an ordinary swing of one lane.
	if LaneRotationRelief <= LaneBlockTime {
		t.Fatalf("LaneRotationRelief %v is shorter than one lane's block (%v) — "+
			"the rotation rule would be waived on every ordinary wait",
			LaneRotationRelief, LaneBlockTime)
	}
}
