package chain

import (
	"bytes"
	"runtime"
	"testing"

	"powmachina/internal/mh"
)

// allocated is how many bytes fn allocates on the heap. The fuzzing engine allocates a little in
// the same process at the same time, so a bound needs some room on top.
func allocated(fn func()) uint64 {
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	fn()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// sampleEveryOther checks every other share, as a light node checks a sample.
type sampleEveryOther struct{ fixedHash }

func (sampleEveryOther) Checks(i int) bool { return i%2 == 0 }

// A settlement body comes from a block, from any peer, and every full node reads it and checks it
// against its chain. No byte may crash the node, a body must never cost much more memory than it
// is long, and what is accepted reads back to the same bytes.
func FuzzSettlement(f *testing.F) {
	_, hs, share := settleChain(f)
	seed := nextBlock(f, hs, share(1, 1, mh.LaneC, 1), share(3, 1, mh.LaneC, 2), share(8, 2, mh.LaneG, 3),
		share(2, 3, mh.LaneG, 4))
	f.Add(seed.Settlement.Serialize())
	f.Add((&Settlement{}).Serialize())
	f.Add([]byte{})
	f.Add([]byte{0xff, 0xff})

	hc, hs, _ := settleChain(f)
	tip := hs[len(hs)-1]
	view, err := hc.SettlementViewOn(tip.ID(), func(id [32]byte) bool { return id[0] == 0 })
	if err != nil {
		f.Fatal(err)
	}
	header := &Header{Version: 1, PrevBlock: tip.ID(), Height: tip.Height + 1, Time: tip.Time + 60}

	f.Fuzz(func(t *testing.T, data []byte) {
		var s *Settlement
		var used int
		var err error
		if n := allocated(func() { s, used, err = DeserializeSettlement(data) }); n > 4*uint64(len(data))+1<<16 {
			t.Fatalf("%d B of input took %d B to read", len(data), n)
		}
		if err != nil {
			return
		}
		if used > len(data) || used > MaxSettlementBytes || len(s.Shares) > MaxSettlementShares {
			t.Fatalf("accepted %d B, %d shares", used, len(s.Shares))
		}
		if !bytes.Equal(s.Serialize(), data[:used]) {
			t.Fatal("the round trip changed the settlement's bytes")
		}
		// Checking it against the chain, as a full node, a light node at the tip and a light node
		// catching up, must not crash either, nor cost much more than the body is long.
		b := &Block{Header: header, Settlement: *s}
		for _, pow := range []PoWHasher{lowHash, sampleEveryOther{lowHash}, fixedHash{0xff}, nil} {
			var paid []SettledShare
			var err error
			if n := allocated(func() { paid, err = b.CheckSettlement(view, pow, RegTest) }); n > 64*uint64(used)+1<<16 {
				t.Fatalf("a body of %d B took %d B to check", used, n)
			}
			if err != nil {
				continue
			}
			if len(paid) != len(s.Shares) {
				t.Fatalf("%d shares, %d paid", len(s.Shares), len(paid))
			}
			if pow == (fixedHash{0xff}) && len(paid) > 0 {
				t.Fatal("shares whose PoW meets no target were paid")
			}
		}
		_, _ = s.ShareIDs(view)
		_ = s.Tally()
	})
}

// A share travels alone too (stratum, RPC): read it from any bytes, back to the same bytes.
func FuzzDeserializeShare(f *testing.F) {
	_, hs, share := settleChain(f)
	_ = hs
	s := share(1, 1, mh.LaneG, 7).Serialize()
	f.Add(s[:])
	f.Add([]byte{})
	f.Add(make([]byte, ShareBytes))

	f.Fuzz(func(t *testing.T, data []byte) {
		s, err := DeserializeShare(data)
		if err != nil {
			return
		}
		if back := s.Serialize(); !bytes.Equal(back[:], data) {
			t.Fatal("the round trip changed the share's bytes")
		}
		_ = s.ID()
		_ = s.Commit.Units()
	})
}
