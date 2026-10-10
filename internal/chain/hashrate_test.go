package chain

import (
	"math/big"
	"testing"

	"powmachina/internal/mh"
)

// The hashrate over time: a sliding window of work divided by time, per lane. A chain with a known
// spacing has to give a known value — otherwise the graph in the explorer would show numbers nobody
// can verify.
func TestHashrateSeries(t *testing.T) {
	g := RegtestGenesis()
	hc := NewHeaderChain(RegTest, g, nil)
	prev := g.ID()
	for i := 1; i <= 60; i++ { // alternating C and G every 10 s → one lane every 20 s
		lane := mh.LaneC
		if i%2 == 0 {
			lane = mh.LaneG
		}
		h := &Header{Version: 1, PrevBlock: prev, Bits: g.Bits, Height: uint32(i),
			Time: g.Time + uint32(i)*10, Lane: lane}
		if err := hc.LoadHeader(h); err != nil {
			t.Fatalf("block %d: %v", i, err)
		}
		prev = h.ID()
	}
	work, err := Work(g.Bits)
	if err != nil {
		t.Fatal(err)
	}
	// a window of 10 lane blocks = 200 s → hashrate = 10 × the block's work / 200 s
	want := new(big.Int).Div(new(big.Int).Mul(work, big.NewInt(10)), big.NewInt(200))

	s := hc.HashrateSeries(1000, 10, 100)
	if s.WindowBlocks != 10 {
		t.Fatalf("window %d", s.WindowBlocks)
	}
	for _, lane := range mh.Lanes {
		pts := s.Lanes[lane]
		if len(pts) < 10 {
			t.Fatalf("lane %s: %d points", lane, len(pts))
		}
		for _, p := range pts {
			if p.Hashrate.Cmp(want) != 0 {
				t.Fatalf("lane %s: hashrate %s, expected %s", lane, p.Hashrate, want)
			}
		}
		if pts[len(pts)-1].Time <= pts[0].Time {
			t.Fatalf("lane %s: the points are not ordered in time", lane)
		}
	}
	// The cap on the number of points, and the last point stays.
	few := hc.HashrateSeries(1000, 2, 5)
	all := hc.HashrateSeries(1000, 2, 1000)
	if n := len(few.Lanes[mh.LaneC]); n > 5 {
		t.Fatalf("the point cap: %d", n)
	}
	last := all.Lanes[mh.LaneC]
	if few.Lanes[mh.LaneC][len(few.Lanes[mh.LaneC])-1].Time != last[len(last)-1].Time {
		t.Fatal("the last point was lost in the thinning")
	}
}

// A young chain: fewer blocks than the window — the graph must neither break nor lie.
func TestHashrateSeriesShortChain(t *testing.T) {
	g := RegtestGenesis()
	hc := NewHeaderChain(RegTest, g, nil)
	if len(hc.HashrateSeries(1000, 10, 100).Lanes[mh.LaneC]) != 0 {
		t.Fatal("an empty chain has an empty graph")
	}
	prev := g.ID()
	for i := 1; i <= 3; i++ {
		h := &Header{Version: 1, PrevBlock: prev, Bits: g.Bits, Height: uint32(i),
			Time: g.Time + uint32(i)*120, Lane: mh.LaneC}
		if err := hc.LoadHeader(h); err != nil {
			t.Fatal(err)
		}
		prev = h.ID()
	}
	// The window shrinks to what is available (3 blocks → window 2 → one point).
	pts := hc.HashrateSeries(1000, 10, 100).Lanes[mh.LaneC]
	if len(pts) != 1 {
		t.Fatalf("a short chain: %d points", len(pts))
	}
	work, _ := Work(g.Bits)
	want := new(big.Int).Div(new(big.Int).Mul(work, big.NewInt(2)), big.NewInt(240))
	if pts[0].Hashrate.Cmp(want) != 0 {
		t.Fatalf("a short chain: %s, expected %s", pts[0].Hashrate, want)
	}
}
