package main

import (
	"math"
	"math/big"
	"math/rand/v2"
	"testing"

	"powmachina/internal/chain"
)

// The baseline of every comparison is the rule the network runs. The simulator has its own copy
// in floating point, so the copy is held to chain.NextBits here: same window, same answer up to
// the precision of the compact encoding.
func TestLwmaIsTheChainsRetarget(t *testing.T) {
	const powLimit = 0x207fffff
	limit, err := chain.CompactToBig(powLimit)
	if err != nil {
		t.Fatal(err)
	}
	toFloat := func(bits uint32) float64 {
		target, err := chain.CompactToBig(bits)
		if err != nil {
			t.Fatal(err)
		}
		f, _ := new(big.Float).Quo(new(big.Float).SetInt(target), new(big.Float).SetInt(limit)).Float64()
		return f
	}
	if lwmaN != chain.LwmaWindow || laneSpacing != chain.LaneBlockTime.Seconds() ||
		relief != int64(chain.LaneRotationRelief.Seconds()) || mtpSpan != chain.MedianTimeSpan {
		t.Fatal("the simulator's constants are not the chain's")
	}

	rng := rand.New(rand.NewPCG(1, 2))
	next := lwma(lwmaOpts{})
	for round := 0; round < 500; round++ {
		s := &sim{}
		var window []chain.LaneBlock
		now := int64(1_700_000_000)
		bits := uint32(0x1d00ffff)
		for i := 0; i <= lwmaN; i++ {
			// Solvetimes from far too short to far too long, some of them backwards.
			now += int64(rng.ExpFloat64()*laneSpacing*math.Pow(4, rng.Float64()*2-1)) - int64(rng.IntN(60))
			if rng.IntN(8) == 0 {
				bits = 0x1d000000 | uint32(0x10000+rng.IntN(0x7effff))
			}
			window = append(window, chain.LaneBlock{Time: uint32(now), Bits: bits})
			s.push(block{lane: laneC, t: now, target: toFloat(bits)})
		}
		wantBits, err := chain.NextBits(window, powLimit)
		if err != nil {
			t.Fatal(err)
		}
		got, want := next(s, laneC), toFloat(wantBits)
		if math.Abs(got/want-1) > 1e-4 { // a compact target can have as few as 17 significant bits
			t.Fatalf("round %d: the simulator says %g, the chain %g", round, got, want)
		}
	}
}
