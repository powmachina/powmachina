package p2p

import (
	"errors"
	"math/rand/v2"
	"testing"

	"powmachina/internal/chain"
	"powmachina/internal/mh"
)

func testShare(miner, job int, nonce uint64, time uint32) *chain.Share {
	s := &chain.Share{BodyRoot: [32]byte{byte(job), byte(job >> 8), 1}}
	s.Commit = chain.ShareCommit{Payout: [32]byte{byte(miner), byte(miner >> 8)}, Threshold: 2, Weight: uint8(nonce % 12)}
	s.Header = chain.Header{Version: 1, PrevBlock: [32]byte{byte(job % 3)}, Time: time, Bits: 0x1d00ffff,
		Height: 100, Nonce: nonce, Lane: mh.Lane(job % 2)}
	s.Header.MerkleRoot = chain.ShareRoot(s.BodyRoot, &s.Commit)
	return s
}

// Shares come out of the decoder exactly as they went into the encoder, across many batches and
// with more keys and jobs than the window holds — the old ones are defined again when they come back.
func TestCompactSharesRoundTrip(t *testing.T) {
	enc, dec := NewShareEncoder(), NewShareDecoder()
	rng := rand.New(rand.NewPCG(1, 2))
	for batch := range 60 {
		var in []*chain.Share
		for range 300 {
			miner := rng.IntN(6000) // more keys than the window
			job := miner*3 + rng.IntN(3)
			in = append(in, testShare(miner, job, rng.Uint64(), 1_800_000_000+uint32(batch)+uint32(rng.IntN(40))))
		}
		out, err := dec.Decode(enc.Encode(in))
		if err != nil {
			t.Fatalf("batch %d: %v", batch, err)
		}
		if len(out) != len(in) {
			t.Fatalf("batch %d: %d shares in, %d out", batch, len(in), len(out))
		}
		for i := range in {
			if in[i].Serialize() != out[i].Serialize() {
				t.Fatalf("batch %d, share %d differs after the round trip", batch, i)
			}
		}
	}
	// The same miners again and again: most shares travel as 14 B.
	enc, dec = NewShareEncoder(), NewShareDecoder()
	var in []*chain.Share
	for i := range 1000 {
		in = append(in, testShare(i%10, i%10, uint64(i), 1_800_000_000+uint32(i%30)))
	}
	payload := enc.Encode(in)
	if _, err := dec.Decode(payload); err != nil {
		t.Fatal(err)
	}
	if per := len(payload) / len(in); per > compactShareBytes+1 {
		t.Fatalf("%d B a share for ten miners, want about %d", per, compactShareBytes)
	}
}

// A job far enough from its first share in time is defined again rather than wrapping the delta.
func TestCompactShareTimeOutOfRange(t *testing.T) {
	enc, dec := NewShareEncoder(), NewShareDecoder()
	a := testShare(1, 1, 1, 1_800_000_000)
	b := testShare(1, 1, 2, 1_800_000_000+40_000)
	out, err := dec.Decode(enc.Encode([]*chain.Share{a, b}))
	if err != nil || len(out) != 2 || out[1].Header.Time != b.Header.Time {
		t.Fatalf("err %v, %d shares", err, len(out))
	}
}

// What no honest encoder writes is refused: a number out of order, a reference to nothing, a
// truncated item, an unknown tag.
func TestCompactSharesRefuseGarbage(t *testing.T) {
	good := NewShareEncoder().Encode([]*chain.Share{testShare(1, 1, 1, 1_800_000_000)})
	cases := map[string][]byte{
		"truncated":   good[:len(good)-1],
		"unknown tag": append(append([]byte{}, good...), 9),
		"share first": good[shareKeyItemBytes+shareJobItemBytes:],
		"job first":   good[shareKeyItemBytes:],
	}
	skipped := append([]byte{}, good...)
	skipped[1] = 5 // the key numbered 5 when 0 is next
	cases["number out of order"] = skipped
	for name, p := range cases {
		if _, err := NewShareDecoder().Decode(p); !errors.Is(err, ErrBadPayload) {
			t.Fatalf("%s: %v, want ErrBadPayload", name, err)
		}
	}
	// The decoder holds no more than the window, however many entries a peer defines.
	enc, dec := NewShareEncoder(), NewShareDecoder()
	for i := range 3 * ShareDictWindow / 1000 {
		var in []*chain.Share
		for j := range 1000 {
			in = append(in, testShare(i*1000+j, i*1000+j, 1, 1_800_000_000))
		}
		if _, err := dec.Decode(enc.Encode(in)); err != nil {
			t.Fatal(err)
		}
	}
	if len(dec.keys) > ShareDictWindow || len(dec.jobs) > ShareDictWindow {
		t.Fatalf("the decoder holds %d keys and %d jobs", len(dec.keys), len(dec.jobs))
	}
}
