package chain

import (
	"bytes"
	"errors"
	"testing"

	"powmachina/internal/mh"
)

func sampleHeader() *Header {
	h := &Header{
		Version: 1, Time: 1_700_000_000, Bits: 0x1d00ffff, Height: 12345,
		Nonce: 0xdeadbeefcafe, Lane: mh.LaneC,
	}
	for i := range h.PrevBlock {
		h.PrevBlock[i] = byte(i)
	}
	for i := range h.MerkleRoot {
		h.MerkleRoot[i] = byte(255 - i)
	}
	return h
}

func TestHeaderRoundTrip(t *testing.T) {
	h := sampleHeader()
	raw := h.Serialize()
	if len(raw) != 89 {
		t.Fatalf("the header should be 89 B, it is %d", len(raw))
	}
	back, err := DeserializeHeader(raw[:])
	if err != nil {
		t.Fatal(err)
	}
	if *back != *h {
		t.Fatalf("the round-trip changed the header:\n%+v\n%+v", back, h)
	}
	if _, err := DeserializeHeader(raw[:88]); err != ErrShortHeader {
		t.Fatalf("a short header should give ErrShortHeader, it gave %v", err)
	}
	bad := raw
	bad[88] = 9
	if _, err := DeserializeHeader(bad[:]); err != ErrBadLane {
		t.Fatalf("an unknown lane should give ErrBadLane, it gave %v", err)
	}
}

// Neither the nonce nor the lane may be in the PoW input — they go into the hash separately.
func TestPoWInputExcludesNonceAndLane(t *testing.T) {
	a := sampleHeader()
	b := sampleHeader()
	b.Nonce = 1
	b.Lane = mh.LaneC
	if ai, bi := a.PoWInput(), b.PoWInput(); !bytes.Equal(ai[:], bi[:]) {
		t.Fatal("the PoW input must not change with the nonce or the lane")
	}
	if a.ID() == b.ID() {
		t.Fatal("the block ID, by contrast, has to change with the nonce and the lane")
	}
}

// The height is in the header on purpose: it determines the DAG epoch and the mix variant.
func TestHeightIsPartOfPoW(t *testing.T) {
	a := sampleHeader()
	b := sampleHeader()
	b.Height++
	if ai, bi := a.PoWInput(), b.PoWInput(); bytes.Equal(ai[:], bi[:]) {
		t.Fatal("changing the height has to change the PoW input")
	}
}

// A verifier for another epoch must not be used — the hash would come out over the wrong DAG.
func TestCheckPoWRejectsWrongEpoch(t *testing.T) {
	v := mh.NewVerifier(0, mh.TestProfile)
	defer v.Close()
	hasher := mh.NewHasher()
	defer hasher.Close()

	h := sampleHeader()
	h.Height = uint32(EpochBlocks * 3) // epoch 3, the verifier has 0
	if _, err := h.CheckPoW(hasher, v); err == nil {
		t.Fatal("a verifier from another epoch has to be refused")
	}
}

// The whole chain: find a nonce that meets a loose target and verify it.
func TestCheckPoWAcceptsFoundNonce(t *testing.T) {
	v := mh.NewVerifier(0, mh.TestProfile)
	defer v.Close()
	hasher := mh.NewHasher()
	defer hasher.Close()

	h := sampleHeader()
	h.Height = 7
	h.Lane = mh.LaneG // the fastest lane, so the test does not take long
	h.Bits = RegtestTargetBits

	found := false
	for nonce := uint64(0); nonce < 20_000; nonce++ {
		h.Nonce = nonce
		ok, err := h.CheckPoW(hasher, v)
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("with the regtest target a nonce has to be found")
	}
	// The same nonce must not be valid in lane C. Lane C is RandomX, so it needs a context — and
	// that also verifies that a hasher without one refuses lane C rather than computing silently.
	h.Lane = mh.LaneC
	if _, err := h.CheckPoW(hasher, v); !errors.Is(err, mh.ErrNoRandomX) {
		t.Fatalf("a hasher without RandomX has to refuse lane C, it gave %v", err)
	}

	rx, err := mh.NewRandomX(0, false) // light — enough for one hash
	if err != nil {
		t.Skipf("RandomX cannot be initialized: %v", err)
	}
	defer rx.Close()
	hrx, err := mh.NewHasherWithRandomX(rx)
	if err != nil {
		t.Fatal(err)
	}
	defer hrx.Close()
	// A **harder target** is needed for the cross-check: with the regtest one (≈ 16 attempts) a
	// nonce from lane G would meet lane C's target with a probability of 1/16 and the test would
	// fail now and then without anything being broken. The point here is that the lanes are two
	// different algorithms, not the difficulty.
	h.Bits = MaxTargetBits
	ok, err := h.CheckPoW(hrx, v)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("a nonce from lane G must not be valid in lane C")
	}
}
