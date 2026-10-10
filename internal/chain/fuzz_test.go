package chain

import (
	"math/big"
	"testing"
)

// The node receives headers from foreign peers. It must not crash on any input — a crash on an
// invalid header is a remote way to take the node down, not a cosmetic bug.
func FuzzDeserializeHeader(f *testing.F) {
	h := sampleHeader()
	raw := h.Serialize()
	f.Add(raw[:])
	f.Add([]byte{})
	f.Add(make([]byte, HeaderBytes))
	f.Add(make([]byte, HeaderBytes*3))

	f.Fuzz(func(t *testing.T, data []byte) {
		hdr, err := DeserializeHeader(data)
		if err != nil {
			return
		}
		// Whatever can be read has to be writable back the same way.
		out := hdr.Serialize()
		again, err := DeserializeHeader(out[:])
		if err != nil {
			t.Fatalf("the round-trip broke the header: %v", err)
		}
		if *again != *hdr {
			t.Fatalf("the round-trip changed the header")
		}
		// The lane always has to be within the allowed range.
		if hdr.Lane > 2 {
			t.Fatalf("an invalid lane %d was accepted", hdr.Lane)
		}
		hdr.ID()
		hdr.PoWInput()
	})
}

// The bits arrive in every header from a foreign peer.
func FuzzCompactTarget(f *testing.F) {
	for _, b := range []uint32{0, 0x1d00ffff, MaxTargetBits, RegtestTargetBits, 0xffffffff, 0x00800000} {
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, bits uint32) {
		n, err := CompactToBig(bits)
		if err != nil {
			return
		}
		if n.Sign() <= 0 {
			t.Fatalf("a non-positive target was accepted from %x", bits)
		}
		if n.BitLen() > 256 {
			t.Fatalf("a target above 2^256 was accepted from %x", bits)
		}
		if _, err := CompactToTarget(bits); err != nil {
			t.Fatalf("CompactToBig passed but CompactToTarget did not: %x", bits)
		}
		if _, err := Work(bits); err != nil {
			t.Fatalf("Work failed on the accepted target %x", bits)
		}
	})
}

// The retarget computes over times and targets from foreign peers.
func FuzzNextBits(f *testing.F) {
	f.Add(uint32(1_700_000_000), uint32(0x1d00ffff), uint32(360), uint32(70))
	f.Add(uint32(0), uint32(0), uint32(0), uint32(0))
	f.Add(^uint32(0), MaxTargetBits, ^uint32(0), uint32(200))

	f.Fuzz(func(t *testing.T, start, bits, spacing, count uint32) {
		n := int(count % 200)
		w := make([]LaneBlock, n)
		ts := start
		for i := range w {
			w[i] = LaneBlock{Time: ts, Bits: bits}
			ts += spacing // the overflow is deliberately allowed — a peer can send anything
		}
		next, err := NextBits(w, MaxTargetBits)
		if err != nil {
			return
		}
		// The result has to be a usable target, not nonsense.
		target, err := CompactToBig(next)
		if err != nil {
			t.Fatalf("the retarget returned an invalid target %x (from %x)", next, bits)
		}
		maxTarget, _ := CompactToBig(MaxTargetBits)
		if target.Cmp(maxTarget) > 0 {
			t.Fatalf("the retarget exceeded the difficulty floor: %x", next)
		}
	})
}

// Finality attestations travel the network from anybody.
func FuzzAttestationVerify(f *testing.F) {
	f.Add([]byte("signature"), uint64(100), uint8(0))
	f.Add(make([]byte, 64), uint64(0), uint8(255))

	q := newQuorumForFuzz()
	f.Fuzz(func(t *testing.T, sig []byte, height uint64, idx uint8) {
		var id [32]byte
		a := Attestation{Height: height, BlockID: id, SignerIdx: idx, Signature: sig}
		if err := a.Verify(q); err == nil {
			t.Fatalf("a random signature passed verification (idx %d, height %d)", idx, height)
		}
		AttestationMessage(height, id)
	})
}

// MESS receives chainwork from foreign branches.
func FuzzAcceptsReorg(f *testing.F) {
	f.Add(int64(1000), int64(2000), int64(3600))
	f.Add(int64(0), int64(0), int64(-1))
	f.Fuzz(func(t *testing.T, local, proposed, age int64) {
		if local < 0 || proposed < 0 {
			return
		}
		AcceptsReorg(big.NewInt(local), big.NewInt(proposed), age)
		MessMultiplier(age)
	})
}
