package chain

import (
	"errors"
	"math"
	"math/big"
	"testing"
	"time"

	"powmachina/internal/mh"
)

const asertTestBits = 0x1e00ffff

func ratio(t *testing.T, bits, of uint32) float64 {
	t.Helper()
	a, err := CompactToBig(bits)
	if err != nil {
		t.Fatal(err)
	}
	b, err := CompactToBig(of)
	if err != nil {
		t.Fatal(err)
	}
	f, _ := new(big.Float).Quo(new(big.Float).SetInt(a), new(big.Float).SetInt(b)).Float64()
	return f
}

func asertNext(t *testing.T, bits uint32, behind int64) uint32 {
	t.Helper()
	next, err := AsertNext(bits, behind, RegtestTargetBits)
	if err != nil {
		t.Fatal(err)
	}
	return next
}

// The numbers the rule is: on pace nothing moves, an hour behind doubles the target, an hour
// ahead halves it, and one gap never moves it by more than sixteen times.
func TestAsertFollowsTheHalfLife(t *testing.T) {
	hour := int64(AsertHalfLife / time.Second)
	if got := asertNext(t, asertTestBits, 0); got != asertTestBits {
		t.Fatalf("on pace the target moved: %08x", got)
	}
	for _, c := range []struct {
		behind int64
		want   float64
	}{
		{hour, 2}, {-hour, 0.5}, {2 * hour, 4}, {hour / 2, math.Sqrt2}, {-hour / 2, 1 / math.Sqrt2},
		{4 * hour, 16}, {40 * hour, 16}, {-4 * hour, 1.0 / 16}, {-40 * hour, 1.0 / 16},
		{math.MaxInt64, 16}, {math.MinInt64, 1.0 / 16},
	} {
		got := ratio(t, asertNext(t, asertTestBits, c.behind), asertTestBits)
		if math.Abs(got/c.want-1) > 2e-4 {
			t.Errorf("%d s behind: the target is %.5f× the last one, expected %.5f×", c.behind, got, c.want)
		}
	}
}

// The cubic stands in for 2^x. It must stay close to it everywhere and never step backwards:
// waiting a second longer can not make a lane harder.
func TestAsertIsMonotoneAndCloseToTheExponential(t *testing.T) {
	hour := float64(AsertHalfLife / time.Second)
	prev := new(big.Int)
	for behind := int64(-4 * 3600); behind <= 4*3600; behind++ {
		next := asertNext(t, asertTestBits, behind)
		target, _ := CompactToBig(next)
		if target.Cmp(prev) < 0 {
			t.Fatalf("%d s behind gives a harder target than %d s", behind, behind-1)
		}
		prev = target
		got, want := ratio(t, next, asertTestBits), math.Pow(2, float64(behind)/hour)
		if math.Abs(got/want-1) > 2e-4 {
			t.Fatalf("%d s behind: %.6f×, 2^x says %.6f×", behind, got, want)
		}
	}
}

// Fixed results. The rule is consensus: a change that moves any of these forks the network.
func TestAsertVectors(t *testing.T) {
	for _, c := range []struct {
		bits   uint32
		behind int64
		want   uint32
	}{
		{0x1e00ffff, 0, 0x1e00ffff},
		{0x1e00ffff, 1, 0x1e01000b},
		{0x1e00ffff, -240, 0x1e00f474},
		{0x1e00ffff, 660, 0x1e0122b4},
		{0x1e00ffff, 3600, 0x1e01fffe},
		{0x1e00ffff, -3600, 0x1d7fff80},
		{0x1e00ffff, 5000, 0x1e029e59},
		{0x1e00ffff, 14400, 0x1e0ffff0},
		{0x1e00ffff, -14400, 0x1d0ffff0},
		{0x1e00ffff, 86400, 0x1e0ffff0},
		{0x1d00ffff, -239, 0x1d00f481},
		{0x1c7fffff, 1799, 0x1d00b4f7},
		{0x1b0404cb, -7201, 0x1b010125},
		{0x1a05db8b, 12345, 0x1a3f1705},
		{0x207fffff, -1, 0x200fffff},
	} {
		if got := asertNext(t, c.bits, c.behind); got != c.want {
			t.Errorf("AsertNext(%08x, %d) = %08x, expected %08x", c.bits, c.behind, got, c.want)
		}
	}
}

func TestAsertStopsAtTheFloor(t *testing.T) {
	if got := asertNext(t, RegtestTargetBits, 3600); got != RegtestTargetBits {
		t.Fatalf("the target went past the floor: %08x", got)
	}
	// A target of one can not get harder, and it must not become zero.
	if got, err := AsertNext(0x01010000, -4*3600, RegtestTargetBits); err != nil || got != 0x01010000 {
		t.Fatalf("the hardest target: %08x, %v", got, err)
	}
}

func FuzzAsertNext(f *testing.F) {
	f.Add(uint32(0x1d00ffff), int64(0))
	f.Add(MaxTargetBits, int64(math.MaxInt64))
	f.Add(uint32(0), int64(math.MinInt64))

	f.Fuzz(func(t *testing.T, bits uint32, behind int64) {
		next, err := AsertNext(bits, behind, MaxTargetBits)
		if err != nil {
			return
		}
		target, err := CompactToBig(next)
		if err != nil || target.Sign() <= 0 {
			t.Fatalf("the retarget returned an unusable target %x (from %x, %d)", next, bits, behind)
		}
		maxTarget, _ := CompactToBig(MaxTargetBits)
		if target.Cmp(maxTarget) > 0 {
			t.Fatalf("the retarget exceeded the difficulty floor: %x", next)
		}
	})
}

// asertChain is a regtest chain whose genesis is hard enough for the difficulty to move both
// ways, and whose clock the test holds.
func asertChain(t *testing.T, params NetParams) (*HeaderChain, *Header, *uint32) {
	t.Helper()
	g := *RegtestGenesis()
	g.Bits = asertTestBits
	hc := NewHeaderChain(params, &g, nil)
	now := g.Time + 30*86400 // the genesis was written a month before the first block
	hc.now = func() time.Time { return time.Unix(int64(now), 0) }
	return hc, &g, &now
}

// The block times in seconds, as the headers carry them.
const (
	netSecs    = uint32(BlockTime / time.Second)
	laneSecs   = uint32(LaneBlockTime / time.Second)
	reliefSecs = uint32(LaneRotationRelief / time.Second)
)

func grow(t *testing.T, hc *HeaderChain, lane mh.Lane, at uint32) *Header {
	t.Helper()
	h := laneHeader(t, hc, lane, at)
	if _, err := hc.AddHeader(h, acceptPoW{}); err != nil {
		t.Fatalf("block %d (%s) at %d: %v", h.Height, lane, at, err)
	}
	return h
}

func nextBits(t *testing.T, hc *HeaderChain, lane mh.Lane) uint32 {
	t.Helper()
	bits, err := hc.NextBitsFor(lane)
	if err != nil {
		t.Fatal(err)
	}
	return bits
}

// The genesis is written long before the network starts. Neither lane may get easier for that
// time: the clock starts at the first block.
func TestAsertClockStartsAtTheFirstBlock(t *testing.T) {
	hc, g, now := asertChain(t, RegTest)
	if got := nextBits(t, hc, mh.LaneC); got != g.Bits {
		t.Fatalf("the first block: %08x, expected the genesis difficulty %08x", got, g.Bits)
	}
	grow(t, hc, mh.LaneC, *now)

	// Both lanes are one block time "ahead" right after the first block, whichever mined it.
	want := asertNext(t, g.Bits, -int64(laneSecs))
	for _, lane := range mh.Lanes {
		if got := nextBits(t, hc, lane); got != want {
			t.Fatalf("lane %s after the first block: %08x, expected %08x", lane, got, want)
		}
	}
}

// A lane that has not mined at all still follows the clock: if the genesis difficulty was set
// too high for it, it comes down while the other lane mines.
func TestLaneWithoutABlockGetsEasierWithTime(t *testing.T) {
	hc, g, now := asertChain(t, RegTest)
	start := *now
	grow(t, hc, mh.LaneC, start)
	*now = start + laneSecs
	grow(t, hc, mh.LaneC, start+laneSecs)
	// Lane C alone from here: one block per relief.
	at := start + laneSecs
	for at < start+3600+laneSecs {
		at += reliefSecs + 1
		*now = at
		grow(t, hc, mh.LaneC, at)
	}
	want := asertNext(t, g.Bits, int64(at-start)-int64(laneSecs))
	if got := nextBits(t, hc, mh.LaneG); got != want {
		t.Fatalf("lane G after %d s without a block: %08x, expected %08x", at-start, got, want)
	}
	if r := ratio(t, want, g.Bits); r < 2 {
		t.Fatalf("an hour of silence eased lane G only %.2f×", r)
	}
}

// What the rule is for. Lane G leaves; lane C keeps the chain going through the relief. Lane
// G's difficulty has to come down with the clock, and lane C's has to stay where it was — its
// blocks are a relief apart because it had to wait, not because it got weaker.
func TestSilentLaneEasesAndTheMiningLaneDoesNot(t *testing.T) {
	hc, _, now := asertChain(t, RegTest)
	at := *now
	for i := 0; i < 40; i++ { // both lanes on pace
		grow(t, hc, mh.Lanes[i%2], at)
		at += netSecs
		*now = at
	}
	gBefore, cBefore := nextBits(t, hc, mh.LaneG), nextBits(t, hc, mh.LaneC)

	// The last block was lane G. Lane C mines two, then one per relief plus its block time, for
	// six hours.
	grow(t, hc, mh.LaneC, at)
	at += laneSecs
	*now = at
	grow(t, hc, mh.LaneC, at)
	cAlone := nextBits(t, hc, mh.LaneC)
	for i := 0; i < int(6*3600/(reliefSecs+laneSecs)); i++ {
		at += reliefSecs + laneSecs
		*now = at
		grow(t, hc, mh.LaneC, at)
		if got := nextBits(t, hc, mh.LaneC); got != cAlone {
			t.Fatalf("relief block %d moved lane C's difficulty: %08x, it was %08x", i+1, got, cAlone)
		}
	}
	if r := ratio(t, cAlone, cBefore); r < 0.9 || r > 1.1 {
		t.Fatalf("lane C alone is %.2f× as easy as it was with lane G around", r)
	}
	// Six hours later lane G is capped at sixteen times easier than its last block.
	if r := ratio(t, nextBits(t, hc, mh.LaneG), gBefore); r < 15 || r > 17 {
		t.Fatalf("lane G after six silent hours: %.2f× easier, expected the cap of 16×", r)
	}
}

// A block dated before its parent is what made a lane look slower than it was.
func TestBlockIsNotDatedBeforeItsParent(t *testing.T) {
	build := func(params NetParams) (*HeaderChain, uint32) {
		hc, _, now := asertChain(t, params)
		at := *now
		for i := 0; i < 12; i++ {
			grow(t, hc, mh.Lanes[i%2], at)
			at += 120
			*now = at
		}
		return hc, at - 120 // the time of the tip
	}

	hc, tip := build(RegTest)
	if _, err := hc.AddHeader(laneHeader(t, hc, mh.LaneC, tip-1), acceptPoW{}); !errors.Is(err, ErrTimeTooOld) {
		t.Fatalf("a block dated a second before its parent: %v, expected ErrTimeTooOld", err)
	}
	if got := hc.MinTime(); got != tip {
		t.Fatalf("the earliest time is %d, the tip is at %d", got, tip)
	}
	if _, err := hc.AddHeader(laneHeader(t, hc, mh.LaneC, tip), acceptPoW{}); err != nil {
		t.Fatalf("a block dated the same as its parent: %v", err)
	}

	// Before the fork the bound was the median of eleven: ten minutes back was fine.
	old, tip := build(noForks(RegTest))
	if _, err := old.AddHeader(laneHeader(t, old, mh.LaneC, tip-599), acceptPoW{}); err != nil {
		t.Fatalf("before the fork a block above the median time past: %v", err)
	}
}

func TestBlockIsAtMostThirtySecondsAhead(t *testing.T) {
	hc, _, now := asertChain(t, RegTest)
	if _, err := hc.AddHeader(laneHeader(t, hc, mh.LaneC, *now+31), acceptPoW{}); !errors.Is(err, ErrTimeTooNew) {
		t.Fatalf("a block 31 s ahead of the clock: %v, expected ErrTimeTooNew", err)
	}
	grow(t, hc, mh.LaneC, *now+30)

	old, _, now := asertChain(t, noForks(RegTest))
	grow(t, old, mh.LaneC, *now+120)
}

// A network that gets the rule at a height: the block before it is retargeted by the window, the
// block at it by the clock, starting from whatever the window left.
func TestLaneDifficultyStartsAtItsHeight(t *testing.T) {
	const forkAt = 30
	params := noForks(RegTest)
	params.Forks = []ForkAt{{Fork: ForkLaneDifficulty, Height: forkAt}}
	hc, g, now := asertChain(t, params)
	at := *now
	var lastC *Header
	for i := 1; i < forkAt; i++ {
		h := grow(t, hc, mh.Lanes[i%2], at)
		if h.Bits != g.Bits {
			t.Fatalf("block %d: the window is not full, the difficulty should not have moved", i)
		}
		if h.Lane == mh.LaneC {
			lastC = h
		}
		at += netSecs
		*now = at
	}
	// Lane C's last block is 28, built on 27, and the tip is 29: a lane block time, on pace.
	if lastC.Height != forkAt-2 {
		t.Fatalf("test assumption: lane C's last block is %d", lastC.Height)
	}
	if got := nextBits(t, hc, mh.LaneC); got != g.Bits {
		t.Fatalf("the first block under the rule, on pace: %08x, expected %08x", got, g.Bits)
	}
	// Lane G's last block is the tip, built a network block time before it: it is half a lane
	// block time ahead.
	if got, want := nextBits(t, hc, mh.LaneG), asertNext(t, g.Bits, int64(netSecs)-int64(laneSecs)); got != want {
		t.Fatalf("lane G at the fork: %08x, expected %08x", got, want)
	}
}
