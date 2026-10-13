package chain

import (
	"testing"

	"powmachina/internal/mh"
)

// knowFork makes the binary know a fork for the length of a test. No real fork exists yet, and
// the mechanism has to be tested before the first one is hung on it.
func knowFork(t *testing.T, f Fork, name string) {
	t.Helper()
	if _, taken := forkNames[f]; taken {
		t.Fatalf("fork %d is a real fork, the test needs a free bit", f)
	}
	forkNames[f] = name
	t.Cleanup(func() { delete(forkNames, f) })
}

// buildVersions builds a header chain where block i+1 has lane lanes[i] and version versions[i].
func buildVersions(t *testing.T, params NetParams, lanes []mh.Lane, versions []uint32) *HeaderChain {
	t.Helper()
	g := RegtestGenesis()
	hc := NewHeaderChain(params, g, nil)
	prev, tm := g.ID(), g.Time
	for i, lane := range lanes {
		tm += 120
		h := &Header{Version: versions[i], PrevBlock: prev, Bits: g.Bits, Height: uint32(i + 1),
			Time: tm, Lane: lane}
		if err := hc.LoadHeader(h); err != nil {
			t.Fatalf("block %d: %v", i+1, err)
		}
		prev = h.ID()
	}
	return hc
}

// A rule holds from its height on, that block included, and a fork that is not in the table
// never holds.
func TestForkIsActiveFromItsHeight(t *testing.T) {
	const scheduled, unscheduled Fork = 3, 4
	p := RegTest
	p.Forks = []ForkAt{{Fork: scheduled, Height: 100}}

	for _, c := range []struct {
		height uint64
		want   bool
	}{{0, false}, {99, false}, {100, true}, {101, true}} {
		if got := p.Active(scheduled, c.height); got != c.want {
			t.Fatalf("height %d: active = %v, expected %v", c.height, got, c.want)
		}
	}
	if p.Active(unscheduled, 1<<40) {
		t.Fatal("a fork with no height became active")
	}
	// A fork at height 0 holds for the genesis: that is how a new network starts with a rule on.
	p.Forks = []ForkAt{{Fork: scheduled, Height: 0}}
	if !p.Active(scheduled, 0) {
		t.Fatal("a fork at height 0 does not hold from the genesis")
	}
}

// The tables that ship have to be well formed, and the networks must not share one slice: a test
// that schedules a fork on regtest would otherwise schedule it on mainnet.
func TestForkTablesAreWellFormed(t *testing.T) {
	for _, p := range []NetParams{MainNet, TestNet, RegTest} {
		if err := p.checkForks(); err != nil {
			t.Fatal(err)
		}
	}
	bad := RegTest
	bad.Forks = []ForkAt{{Fork: 2, Height: 10}, {Fork: 2, Height: 20}}
	if bad.checkForks() == nil {
		t.Fatal("a fork with two heights passed")
	}
	bad.Forks = []ForkAt{{Fork: MaxForks, Height: 10}}
	if bad.checkForks() == nil {
		t.Fatal("a fork past the signal bits passed")
	}
}

// With no fork known the version is the plain base: the blocks of today stay what they were.
func TestBlockVersionWithoutForksIsTheBase(t *testing.T) {
	for _, p := range []NetParams{MainNet, TestNet, RegTest} {
		if v := p.BlockVersion(1); v != VersionBase {
			t.Fatalf("%s: version %#x, expected %#x", p.Name, v, VersionBase)
		}
	}
}

// A known fork is signalled until it is active — also when the network has no height for it yet,
// because the signalling is what a height is chosen by.
func TestBlockVersionSignalsUntilActive(t *testing.T) {
	const f Fork = 5
	knowFork(t, f, "test-rule")
	bit := uint32(1) << (versionSignalShift + uint32(f))

	p := RegTest
	if v := p.BlockVersion(1); v != VersionBase|bit {
		t.Fatalf("no height yet: version %#x, expected the bit set", v)
	}
	p.Forks = []ForkAt{{Fork: f, Height: 50}}
	if v := p.BlockVersion(49); v != VersionBase|bit {
		t.Fatalf("one block before the fork: version %#x, expected the bit set", v)
	}
	if v := p.BlockVersion(50); v != VersionBase {
		t.Fatalf("at the fork: version %#x, expected the base", v)
	}
	if !SignalsFork(VersionBase|bit, f) || SignalsFork(VersionBase, f) {
		t.Fatal("SignalsFork does not read what BlockVersion writes")
	}
	// The base itself must never read as a signal.
	for g := Fork(0); g < MaxForks; g++ {
		if SignalsFork(VersionBase, g) {
			t.Fatalf("the base version signals fork %d", g)
		}
	}
}

// The count is per lane: with a guaranteed share for each lane, "70 % of the blocks" says nothing
// when all of them come from one.
func TestForkStatusCountsPerLane(t *testing.T) {
	const known, foreign Fork = 2, 9
	knowFork(t, known, "test-rule")
	p := RegTest
	p.Forks = []ForkAt{{Fork: known, Height: 1000}}

	ready := VersionBase | 1<<(versionSignalShift+uint32(known))
	other := VersionBase | 1<<(versionSignalShift+uint32(foreign))
	// 12 blocks, C and G in turn. Every C block signals the known fork, no G block does; one G
	// block signals a bit this binary has no rule for.
	lanes := make([]mh.Lane, 12)
	versions := make([]uint32, 12)
	for i := range lanes {
		lanes[i], versions[i] = mh.LaneC, ready
		if i%2 == 1 {
			lanes[i], versions[i] = mh.LaneG, VersionBase
		}
	}
	versions[11] = other

	st := buildVersions(t, p, lanes, versions).ForkStatus()
	if st.TipHeight != 12 || st.Window != 12 {
		t.Fatalf("height %d, window %d, expected 12 and 12", st.TipHeight, st.Window)
	}
	if st.Blocks[mh.LaneC] != 6 || st.Blocks[mh.LaneG] != 6 {
		t.Fatalf("blocks per lane %v, expected 6 and 6", st.Blocks)
	}
	if len(st.Forks) != 2 {
		t.Fatalf("%d forks reported, expected the known one and the foreign one", len(st.Forks))
	}
	k, u := st.Forks[0], st.Forks[1]
	if k.Fork != known || !k.Known || !k.Scheduled || k.Height != 1000 || k.Active {
		t.Fatalf("the known fork: %+v", k)
	}
	if k.Signalling[mh.LaneC] != 6 || k.Signalling[mh.LaneG] != 0 {
		t.Fatalf("the known fork is signalled by %v, expected 6 C and 0 G", k.Signalling)
	}
	if u.Fork != foreign || u.Known || u.Scheduled || u.Active {
		t.Fatalf("the foreign bit: %+v", u)
	}
	if u.Signalling[mh.LaneG] != 1 || u.Signalling[mh.LaneC] != 0 {
		t.Fatalf("the foreign bit is signalled by %v, expected one G block", u.Signalling)
	}
}

// The status says "active" for the block that comes next, not for the tip: that is the block a
// miner is about to build.
func TestForkStatusActiveIsForTheNextBlock(t *testing.T) {
	const f Fork = 1
	knowFork(t, f, "test-rule")
	p := RegTest
	p.Forks = []ForkAt{{Fork: f, Height: 4}}

	lanes := []mh.Lane{mh.LaneC, mh.LaneG, mh.LaneC}
	versions := []uint32{p.BlockVersion(1), p.BlockVersion(2), p.BlockVersion(3)}
	hc := buildVersions(t, p, lanes, versions)
	if st := hc.ForkStatus(); len(st.Forks) != 1 || !st.Forks[0].Active {
		t.Fatalf("tip 3, fork at 4: %+v", st.Forks)
	}
	if st := buildVersions(t, p, lanes[:2], versions[:2]).ForkStatus(); st.Forks[0].Active {
		t.Fatal("tip 2, fork at 4: reported active one block early")
	}
}

// The window is bounded: a long chain is not walked to the genesis on every request.
func TestForkStatusWindowIsBounded(t *testing.T) {
	n := SignalWindow + 40
	lanes := make([]mh.Lane, n)
	versions := make([]uint32, n)
	for i := range lanes {
		lanes[i], versions[i] = mh.Lane(i%2), VersionBase
	}
	st := buildVersions(t, RegTest, lanes, versions).ForkStatus()
	if st.Window != SignalWindow {
		t.Fatalf("window %d, expected %d", st.Window, SignalWindow)
	}
	if len(st.Forks) != 0 {
		t.Fatalf("no fork is known and none is signalled, got %+v", st.Forks)
	}
}
