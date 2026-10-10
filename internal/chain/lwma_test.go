package chain

import (
	"math/big"
	"testing"
)

func steadyWindow(bits uint32, spacing int64) []LaneBlock {
	w := make([]LaneBlock, LwmaWindow+1)
	for i := range w {
		w[i] = LaneBlock{Time: uint32(int64(1_700_000_000) + int64(i)*spacing), Bits: bits}
	}
	return w
}

// At exactly the target pace the difficulty must not move (apart from rounding).
func TestLwmaStableAtTargetSpacing(t *testing.T) {
	const bits uint32 = 0x1d00ffff
	got, err := NextBits(steadyWindow(bits, int64(LaneBlockTime.Seconds())), MaxTargetBits)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := CompactToBig(bits)
	have, _ := CompactToBig(got)
	diff := new(big.Int).Sub(have, want)
	diff.Abs(diff)
	tol := new(big.Int).Div(want, big.NewInt(100)) // 1 %
	if diff.Cmp(tol) > 0 {
		t.Fatalf("the target moved by more than 1 %% at the exact pace: %x → %x", bits, got)
	}
}

// Faster blocks have to raise the difficulty (the target falls), slower ones the other way around.
func TestLwmaRespondsToSpacing(t *testing.T) {
	const bits uint32 = 0x1d00ffff
	base, _ := CompactToBig(bits)

	fast, err := NextBits(steadyWindow(bits, int64(LaneBlockTime.Seconds())/2), MaxTargetBits)
	if err != nil {
		t.Fatal(err)
	}
	fastTarget, _ := CompactToBig(fast)
	if fastTarget.Cmp(base) >= 0 {
		t.Fatalf("with twice the hashrate the target has to fall: %x → %x", bits, fast)
	}

	slow, err := NextBits(steadyWindow(bits, int64(LaneBlockTime.Seconds())*2), MaxTargetBits)
	if err != nil {
		t.Fatal(err)
	}
	slowTarget, _ := CompactToBig(slow)
	if slowTarget.Cmp(base) <= 0 {
		t.Fatalf("with half the hashrate the target has to rise: %x → %x", bits, slow)
	}
}

// A difficulty jump is capped at ×4 / ÷4 — against an attack through manipulated times.
func TestLwmaClampsJumps(t *testing.T) {
	const bits uint32 = 0x1d00ffff
	base, _ := CompactToBig(bits)

	// Extremely slow blocks: 100× the target time.
	slow, err := NextBits(steadyWindow(bits, int64(LaneBlockTime.Seconds())*100), MaxTargetBits)
	if err != nil {
		t.Fatal(err)
	}
	slowTarget, _ := CompactToBig(slow)
	limit := new(big.Int).Mul(base, big.NewInt(maxAdjustFactor))
	limit.Add(limit, new(big.Int).Div(limit, big.NewInt(100))) // 1 % rounding tolerance
	if slowTarget.Cmp(limit) > 0 {
		t.Fatalf("the target jumped past the ×4 cap: %x → %x", bits, slow)
	}

	// Blocks found instantly (the same time) → the solvetime is clipped to 1 s.
	fast, err := NextBits(steadyWindow(bits, 0), MaxTargetBits)
	if err != nil {
		t.Fatal(err)
	}
	fastTarget, _ := CompactToBig(fast)
	floor := new(big.Int).Div(base, big.NewInt(maxAdjustFactor))
	floor.Sub(floor, new(big.Int).Div(floor, big.NewInt(100)))
	if fastTarget.Cmp(floor) < 0 {
		t.Fatalf("the target fell below ÷4: %x → %x", bits, fast)
	}
}

// The difficulty must not rise above the floor — a dead lane has to come back within an ordinary miner's reach.
func TestLwmaRespectsFloor(t *testing.T) {
	got, err := NextBits(steadyWindow(MaxTargetBits, int64(LaneBlockTime.Seconds())*10), MaxTargetBits)
	if err != nil {
		t.Fatal(err)
	}
	maxTarget, _ := CompactToBig(MaxTargetBits)
	target, _ := CompactToBig(got)
	if target.Cmp(maxTarget) > 0 {
		t.Fatalf("the target exceeded the floor: %x > %x", got, MaxTargetBits)
	}
}

// A short history at the start of the chain does not break the retarget.
func TestLwmaShortHistory(t *testing.T) {
	const bits uint32 = 0x1d00ffff
	w := steadyWindow(bits, 360)[:5]
	got, err := NextBits(w, MaxTargetBits)
	if err != nil {
		t.Fatal(err)
	}
	if got != bits {
		t.Fatalf("with a short history the original target should stay, got %x", got)
	}
	// But a target above the floor must not be repeated even with a short history.
	above := steadyWindow(0x1f010029, 360)[:5]
	if got, err := NextBits(above, MaxTargetBits); err != nil || got != MaxTargetBits {
		t.Fatalf("a target above the floor has to be clipped to the floor, got %x (%v)", got, err)
	}
	if _, err := NextBits(w[:1], MaxTargetBits); err != ErrShortWindow {
		t.Fatalf("with a single block I expect ErrShortWindow, got %v", err)
	}
}

// BigToCompact rounds up, so clipping to the floor before encoding could push the result just
// above the floor. FuzzNextBits covers the same ground at random.
func TestLwmaFloorSurvivesRounding(t *testing.T) {
	maxTarget, _ := CompactToBig(MaxTargetBits)
	// A window with extremely slow blocks pushes the target above the floor.
	for _, spacing := range []int64{3600, 86400, 1 << 20} {
		got, err := NextBits(steadyWindow(MaxTargetBits, spacing), MaxTargetBits)
		if err != nil {
			t.Fatal(err)
		}
		target, err := CompactToBig(got)
		if err != nil {
			t.Fatalf("the retarget returned an invalid target %x", got)
		}
		if target.Cmp(maxTarget) > 0 {
			t.Fatalf("the target %x exceeded the floor %x (spacing %d s)", got, MaxTargetBits, spacing)
		}
	}
}

// Every network has its own difficulty floor — regtest has to stay trivial.
func TestPowLimitIsPerNetwork(t *testing.T) {
	got, err := NextBits(steadyWindow(RegtestTargetBits, 360)[:5], RegTest.PowLimitBits)
	if err != nil {
		t.Fatal(err)
	}
	if got != RegtestTargetBits {
		t.Fatalf("on regtest the regtest target should stay, got %x", got)
	}
	// The same input hits a harder floor on mainnet.
	got, err = NextBits(steadyWindow(RegtestTargetBits, 360)[:5], MainNet.PowLimitBits)
	if err != nil {
		t.Fatal(err)
	}
	if got != MaxTargetBits {
		t.Fatalf("on mainnet it has to be clipped to the floor, got %x", got)
	}
}
