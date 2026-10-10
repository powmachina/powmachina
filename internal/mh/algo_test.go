package mh

import (
	"math/bits"
	"testing"
)

// Properties of the algorithm that have to hold regardless of the stored vectors. The vectors say
// "this is the same function as before"; these tests say "and it is the function we want".

func testHeader(seed byte) *[HeaderBytes]byte {
	var h [HeaderBytes]byte
	for i := range h {
		h[i] = byte(i)*seed + seed
	}
	return &h
}

// The light path and the full dataset have to give a bit-identical result — otherwise the network
// falls apart into miners and light clients.
func TestLightAndFullDagAgree(t *testing.T) {
	p := TestDag(0)
	cache := BuildCache(p)
	full := BuildFullDagFrom(cache)
	light := NewLightDagFrom(cache)
	math := NewRandMath(0)
	h := NewHasher()

	// Lane C does not go into the DAG at all (it is RandomX), so only lane G is compared.
	a, err := h.HashWithDag(full, math, testHeader(3), 42, LaneG)
	if err != nil {
		t.Fatal(err)
	}
	b, err := h.HashWithDag(light, math, testHeader(3), 42, LaneG)
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatalf("light != full for lane G:\n  full  %x\n  light %x", a, b)
	}
}

// The same input has to give the same hash even after other hashes in between — both the VM and the
// mix have internal state and it must not carry over.
func TestDeterministicAcrossReuse(t *testing.T) {
	v := NewVerifier(0, TestProfile)
	defer v.Close()
	rx, err := NewRandomX(0, false)
	if err != nil {
		t.Skipf("RandomX cannot be initialized: %v", err)
	}
	defer rx.Close()
	h, err := NewHasherWithRandomX(rx)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	for _, lane := range Lanes {
		a, err := h.Hash(v, testHeader(7), 1, lane)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := h.Hash(v, testHeader(9), 2, LaneG); err != nil {
			t.Fatal(err)
		}
		if _, err := h.Hash(v, testHeader(9), 3, LaneC); err != nil {
			t.Fatal(err)
		}
		b, err := h.Hash(v, testHeader(7), 1, lane)
		if err != nil {
			t.Fatal(err)
		}
		if a != b {
			t.Fatalf("repeating changes the result for lane %s", lane)
		}
	}
}

// The lane is part of the input — a nonce found in one lane must not be valid in another.
func TestLanesAreDomainSeparated(t *testing.T) {
	v := NewVerifier(0, TestProfile)
	defer v.Close()
	rx, err := NewRandomX(0, false)
	if err != nil {
		t.Skipf("RandomX cannot be initialized: %v", err)
	}
	defer rx.Close()
	h, err := NewHasherWithRandomX(rx)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	c, err := h.Hash(v, testHeader(1), 5, LaneC)
	if err != nil {
		t.Fatal(err)
	}
	g, err := h.Hash(v, testHeader(1), 5, LaneG)
	if err != nil {
		t.Fatal(err)
	}
	if c == g {
		t.Fatal("the lane has to be in the preimage, otherwise a nonce carries between lanes")
	}

	// And it really has to be in its place in the preimage.
	p := PowPreimage(testHeader(1), 5, LaneG)
	if len(p) != 89 || p[LaneOffset] != byte(LaneG) {
		t.Fatalf("the preimage has %d B and the lane at byte %d", len(p), LaneOffset)
	}
}

// Changing the epoch changes the RANDMATH sequence and the dataset → a different result.
func TestEpochChangesResult(t *testing.T) {
	full0 := BuildFullDag(TestDag(0))
	full1 := BuildFullDag(TestDag(1))
	h := NewHasher()

	a, err := h.HashWithDag(full0, NewRandMath(0), testHeader(2), 3, LaneG)
	if err != nil {
		t.Fatal(err)
	}
	b, err := h.HashWithDag(full1, NewRandMath(1), testHeader(2), 3, LaneG)
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("the epoch does not change lane G's result")
	}
	// Lane C does not go into the DAG — its epoch is carried by the RandomX key.
	if RxEpochKey(0) == RxEpochKey(1) {
		t.Fatal("the epoch does not change the RandomX key")
	}
}

// The avalanche effect: changing one bit of the nonce changes ~half the bits of the output.
func TestAvalancheOnNonce(t *testing.T) {
	full := BuildFullDag(TestDag(0))
	math := NewRandMath(0)
	h := NewHasher()

	const trials = 16
	total := 0
	for n := range uint64(trials) {
		a, err := h.HashWithDag(full, math, testHeader(4), n, LaneG)
		if err != nil {
			t.Fatal(err)
		}
		b, err := h.HashWithDag(full, math, testHeader(4), n^1, LaneG)
		if err != nil {
			t.Fatal(err)
		}
		for i := range a {
			total += bits.OnesCount8(a[i] ^ b[i])
		}
	}
	avg := float64(total) / trials
	if avg < 108 || avg > 148 {
		t.Fatalf("a weak avalanche effect: %.1f bits out of 256", avg)
	}
}

// The DAG grows, but never above the cap — otherwise the algorithm would retire cards after years.
func TestDagGrowthIsCapped(t *testing.T) {
	if got := dagBytesForEpoch(0); got != DagBytesMainnetEpoch0 {
		t.Fatalf("epoch 0 should have %d B, it has %d", DagBytesMainnetEpoch0, got)
	}
	if dagBytesForEpoch(1) <= dagBytesForEpoch(0) {
		t.Fatal("the DAG does not grow")
	}
	if dagBytesForEpoch(1_000_000) != DagMaxBytes {
		t.Fatal("the DAG outgrew the cap")
	}
	// with a 120 s block and an epoch of 5 000 blocks: 5 years ≈ 263 epochs, 10 years ≈ 526
	if dagBytesForEpoch(263) >= 3<<30 {
		t.Fatal("after 5 years the DAG should stay below 3 GiB")
	}
	if dagBytesForEpoch(526) >= 4<<30 {
		t.Fatal("after 10 years the DAG should stay below 4 GiB")
	}
	// the 8 GiB cap is reached in epoch 1 792 (≈ 34 years)
	if dagBytesForEpoch(1791) >= DagMaxBytes || dagBytesForEpoch(1792) != DagMaxBytes {
		t.Fatal("the cap is not reached in epoch 1 792")
	}
}

// Lane G has to read enough from the DAG that it cannot be mined the light way. The cap is the cost
// of verification: with 4 096 items a light verification of lane G took 46 ms (ALGO §7.5).
func TestLaneGDagReadsAreInRange(t *testing.T) {
	items := LaneGWork().DagBytes / DagItemBytes
	if items != 4096 {
		t.Fatalf("changing the number of DAG accesses is a consensus change: %d", items)
	}
}

// The RANDMATH sequence has to change with the epoch — otherwise an ASIC could hardwire it.
func TestRandMathChangesWithEpoch(t *testing.T) {
	a := NewRandMath(0).OpsRaw()
	b := NewRandMath(1).OpsRaw()
	if a == b {
		t.Fatal("RANDMATH is the same in two epochs")
	}
	// All 9 kinds of operation have to be reachable.
	seen := map[uint8]bool{}
	for e := range uint64(64) {
		for _, op := range NewRandMath(e).OpsRaw() {
			seen[op[0]] = true
		}
	}
	if len(seen) != 9 {
		t.Fatalf("in 64 epochs only %d of the 9 kinds of operation appeared", len(seen))
	}
}

// The cache and the dataset have to have exactly the sizes the profile promises.
func TestDagParamSizes(t *testing.T) {
	p := TestDag(0)
	if p.CacheItems() != (1<<20)/CacheItemBytes || p.DatasetItems() != (8<<20)/DagItemBytes {
		t.Fatalf("the test profile has the wrong counts: %d / %d", p.CacheItems(), p.DatasetItems())
	}
	c := BuildCache(p)
	if len(c.Words()) != p.CacheItems()*CacheItemWords {
		t.Fatalf("the cache has %d words", len(c.Words()))
	}
	d := BuildFullDagFrom(c)
	if d.Bytes() != p.DatasetBytes {
		t.Fatalf("the dataset has %d B, expected %d", d.Bytes(), p.DatasetBytes)
	}
}
