package chain

import (
	"testing"
	"time"
)

func TestSubsidySchedule(t *testing.T) {
	if got := Subsidy(0); got != 0 {
		t.Fatalf("the genesis has to have a zero reward, it has %d", got)
	}
	// The slow start: the first block 1 % of 10 POWM = 0.1 POWM, the last block of the ramp the full 10.
	if got, want := Subsidy(1), 10*POWM/100; got != want {
		t.Fatalf("the first block: got %d, want %d", got, want)
	}
	if got := Subsidy(SlowStartBlocks - 1); got != InitialSubsidy {
		t.Fatalf("the end of the slow start has to give the full reward, it gives %d", got)
	}
	if got := Subsidy(SlowStartBlocks); got != InitialSubsidy {
		t.Fatalf("after the slow start the reward has to be full, it is %d", got)
	}
	// The ramp has to grow monotonically.
	prev := int64(-1)
	for h := uint64(1); h < SlowStartBlocks; h++ {
		s := Subsidy(h)
		if s < prev {
			t.Fatalf("the reward dropped at height %d: %d < %d", h, s, prev)
		}
		prev = s
	}
	// Halving.
	if got, want := Subsidy(HalvingInterval), InitialSubsidy/2; got != want {
		t.Fatalf("the first halving: got %d, want %d", got, want)
	}
	if got, want := Subsidy(3*HalvingInterval), InitialSubsidy/8; got != want {
		t.Fatalf("the third halving: got %d, want %d", got, want)
	}
}

// Halvings emit ~84 M POWM (the slow start a bit less) — that part of the emission stays as in the
// original plan. It is counted over the halvings above the permanent reward.
func TestHalvingEmission(t *testing.T) {
	var total int64
	for k := uint(0); k < 63; k++ {
		reward := InitialSubsidy >> k
		if reward < TailSubsidy {
			break
		}
		total += reward * int64(HalvingInterval)
	}
	var deficit int64
	for h := uint64(0); h < SlowStartBlocks; h++ {
		deficit += InitialSubsidy - Subsidy(h)
	}
	emitted := total - deficit

	if emitted > HalvingSupply {
		t.Fatalf("the emission through halvings %d exceeds %d", emitted, HalvingSupply)
	}
	// five halvings above the permanent reward: 10, 5, 2.5, 1.25, 0.625 and 0.3125 → ~82 M
	if emitted < 80_000_000*POWM {
		t.Fatalf("the emission through halvings is only %d POWM", emitted/POWM)
	}
	t.Logf("halvings emit %d POWM, then a permanent reward of %d cogs per block",
		emitted/POWM, TailSubsidy)
}

// The permanent reward takes over where halving would drop below it, and stays forever.
func TestTailSubsidy(t *testing.T) {
	cases := []struct {
		height uint64
		want   int64
	}{
		{5*HalvingInterval - 1, InitialSubsidy >> 4}, // 0.625 POWM
		{5 * HalvingInterval, InitialSubsidy >> 5},   // 0.3125 POWM — still above the permanent one
		{6*HalvingInterval - 1, InitialSubsidy >> 5},
		{6 * HalvingInterval, TailSubsidy},  // 0.15625 would be below the permanent one → 0.24975
		{20 * HalvingInterval, TailSubsidy}, // ~80 years
		{63 * HalvingInterval, TailSubsidy}, // once upon a time zero
		{1 << 62, TailSubsidy},
	}
	for _, c := range cases {
		if got := Subsidy(c.height); got != c.want {
			t.Errorf("height %d: reward %d, expected %d", c.height, got, c.want)
		}
	}
	// The slow start does not change — the first block pays 0.10 POWM, not the permanent reward.
	if got := Subsidy(1); got != InitialSubsidy/100 {
		t.Errorf("the first block: %d, expected %d", got, InitialSubsidy/100)
	}
	// 0.999 POWM every 2 minutes, whatever the block time.
	if perTwoMinutes := TailSubsidy * int64(2*time.Minute/BlockTime); perTwoMinutes != 99_900_000 {
		t.Errorf("the permanent reward is %d cogs every 2 minutes — the agreed value is 0.999 POWM", perTwoMinutes)
	}
}

func TestDevFee(t *testing.T) {
	if got := DevFee(0); got != 0 {
		t.Fatalf("the genesis has no dev fee")
	}
	h := SlowStartBlocks + 1
	if got, want := DevFee(h), InitialSubsidy*5/100; got != want {
		t.Fatalf("dev fee: got %d, want %d", got, want)
	}
	if got := DevFee(DevFeeEndHeight); got != 0 {
		t.Fatalf("after DevFeeEndHeight the dev fee has to be zero, it is %d", got)
	}
	if got, want := MinerSubsidy(h), Subsidy(h)-DevFee(h); got != want {
		t.Fatalf("the miner should be left %d, they get %d", want, got)
	}
	// The total volume of the dev fee.
	var total int64
	for i := uint64(0); i < DevFeeEndHeight; i++ {
		total += DevFee(i)
	}
	t.Logf("dev fee in total: %d POWM", total/POWM)
	if total > 1_100_000*POWM {
		t.Fatalf("a dev fee of %d POWM is more than the spec expects", total/POWM)
	}
}
