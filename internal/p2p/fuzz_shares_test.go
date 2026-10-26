package p2p

import (
	"bytes"
	"runtime"
	"testing"

	"powmachina/internal/chain"
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

// Compact shares come from any peer, in messages that build on the dictionaries of the earlier
// ones: two messages through one decoder, cut anywhere. No byte may crash the node, a message must
// not cost much more memory than it is long, the dictionaries stay at their window, and every share
// that comes out is a whole one that travels again unchanged.
func FuzzShareDecoder(f *testing.F) {
	enc := NewShareEncoder()
	first := enc.Encode([]*chain.Share{testShare(1, 1, 1, 1_800_000_000), testShare(2, 4, 2, 1_800_000_010)})
	second := enc.Encode([]*chain.Share{testShare(1, 1, 3, 1_800_000_020), testShare(3, 9, 4, 1_800_000_030)})
	f.Add(append(append([]byte{}, first...), second...), uint16(len(first)))
	f.Add(first, uint16(0))
	f.Add([]byte{itemShare, 0, 0}, uint16(1))
	f.Add([]byte{}, uint16(0))

	f.Fuzz(func(t *testing.T, data []byte, cut uint16) {
		at := min(int(cut), len(data))
		dec := NewShareDecoder()
		for _, payload := range [][]byte{data[:at], data[at:]} {
			var out []*chain.Share
			var err error
			if n := allocated(func() { out, err = dec.Decode(payload) }); n > 32*uint64(len(payload))+1<<16 {
				t.Fatalf("a message of %d B took %d B to read", len(payload), n)
			}
			if len(dec.keys) > ShareDictWindow || len(dec.jobs) > ShareDictWindow {
				t.Fatalf("the decoder holds %d keys and %d jobs", len(dec.keys), len(dec.jobs))
			}
			if err != nil {
				return // the connection is dropped
			}
			if len(out) > MaxShareItemsPerMessage {
				t.Fatalf("%d shares from one message", len(out))
			}
			for _, s := range out {
				if s.Header.MerkleRoot != chain.ShareRoot(s.BodyRoot, &s.Commit) {
					t.Fatal("a share whose header does not commit to its body root and commitment")
				}
				raw := s.Serialize()
				back, err := chain.DeserializeShare(raw[:])
				if err != nil || back.Serialize() != raw {
					t.Fatalf("a decoded share does not read back: %v", err)
				}
			}
			// What came out travels again: a fresh pair of ends gives the same shares.
			again, err := NewShareDecoder().Decode(NewShareEncoder().Encode(out))
			if err != nil || len(again) != len(out) {
				t.Fatalf("re-encoded: %d of %d shares, %v", len(again), len(out), err)
			}
			for i := range out {
				if again[i].Serialize() != out[i].Serialize() {
					t.Fatalf("share %d changed on the way again", i)
				}
			}
		}
	})
}

// A proof of double signing is gossiped and stored. Read it from any bytes: what is accepted is two
// attestations of one key at one height for two blocks, and it travels back unchanged.
func FuzzDecodeEquivocation(f *testing.F) {
	a := chain.Attestation{Height: 9, BlockID: [32]byte{1}, Key: [32]byte{7}, Signature: make([]byte, 64)}
	b := a
	b.BlockID = [32]byte{2}
	good, _ := EncodeEquivocation(&chain.Equivocation{Key: a.Key, Height: a.Height, First: a, Second: b})
	f.Add(good)
	f.Add(make([]byte, EquivocationBytes))
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, data []byte) {
		e, err := DecodeEquivocation(data)
		if err != nil {
			return
		}
		if e.First.Key != e.Key || e.Second.Key != e.Key || e.First.Height != e.Height ||
			e.Second.Height != e.Height || e.First.BlockID == e.Second.BlockID {
			t.Fatalf("an inconsistent proof was accepted: %+v", e)
		}
		back, err := EncodeEquivocation(e)
		if err != nil || !bytes.Equal(back, data) {
			t.Fatalf("the round trip changed the proof's bytes: %v", err)
		}
	})
}
