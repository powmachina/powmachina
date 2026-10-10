package chain

import (
	"math/big"
	"testing"
)

func TestGeoMean2(t *testing.T) {
	// An exact root has to come out exactly, otherwise rounded down: r^2 ≤ a*b < (r+1)^2.
	for _, pair := range [][2]int64{{0, 0}, {1, 1}, {4, 9}, {2, 3}, {7, 7}, {1, 1_000_000}} {
		a, b := big.NewInt(pair[0]), big.NewInt(pair[1])
		r := GeoMean2(a, b)
		prod := new(big.Int).Mul(a, b)
		sq := new(big.Int).Mul(r, r)
		next := new(big.Int).Add(r, big.NewInt(1))
		nextSq := new(big.Int).Mul(next, next)
		if sq.Cmp(prod) > 0 || nextSq.Cmp(prod) <= 0 {
			t.Fatalf("geomean(%d,%d) = %s is wrong", pair[0], pair[1], r)
		}
	}
	// Big numbers in the chainwork scale.
	for _, shift := range []uint{300, 768, 2200} {
		x := new(big.Int).Lsh(big.NewInt(1), shift)
		r := GeoMean2(x, x)
		if r.Cmp(x) != 0 {
			t.Fatalf("geomean(2^%d, 2^%d) should be 2^%d", shift, shift, shift)
		}
	}
}

// When every lane has the same average work, normalization must not change anything.
func TestNormalizedWorkNeutral(t *testing.T) {
	const bits uint32 = 0x1d00ffff
	w, err := Work(bits)
	if err != nil {
		t.Fatal(err)
	}
	geo := GeoMean2(w, w)
	got, err := NormalizedWork(bits, w, geo)
	if err != nil {
		t.Fatal(err)
	}
	if got.Cmp(w) != 0 {
		t.Fatalf("with equal averages the work has to stay the same: %s vs %s", got, w)
	}
}

// A lane with a hundred times higher average work has to have its block weighted a hundred times
// less — otherwise an attacker would buy chainwork in the cheapest lane.
func TestNormalizedWorkBalancesLanes(t *testing.T) {
	const bits uint32 = 0x1d00ffff
	w, _ := Work(bits)

	cheap := new(big.Int).Set(w)                 // lane C
	rich := new(big.Int).Mul(w, big.NewInt(100)) // lane G, 100× more work per block
	geo := GeoMean2(cheap, rich)

	nc, err := NormalizedWork(bits, cheap, geo)
	if err != nil {
		t.Fatal(err)
	}
	richBits := BigToCompact(func() *big.Int {
		tgt, _ := CompactToBig(bits)
		return new(big.Int).Div(tgt, big.NewInt(100)) // a 100× harder target
	}())
	ng, err := NormalizedWork(richBits, rich, geo)
	if err != nil {
		t.Fatal(err)
	}

	// After normalization both blocks have to weigh roughly the same (within 2 %).
	diff := new(big.Int).Sub(nc, ng)
	diff.Abs(diff)
	tol := new(big.Int).Div(nc, big.NewInt(50))
	if diff.Cmp(tol) > 0 {
		t.Fatalf("blocks of different lanes weigh differently after normalization: %s vs %s", nc, ng)
	}
}

func TestNormalizedWorkRejectsZeroAverage(t *testing.T) {
	if _, err := NormalizedWork(0x1d00ffff, big.NewInt(0), big.NewInt(1)); err != ErrNoAverage {
		t.Fatalf("a zero average should give ErrNoAverage, it gave %v", err)
	}
}

func TestCompactRoundTrip(t *testing.T) {
	for _, bits := range []uint32{0x1d00ffff, 0x1e00ffff, MaxTargetBits, 0x1b0404cb} {
		n, err := CompactToBig(bits)
		if err != nil {
			t.Fatalf("%x: %v", bits, err)
		}
		if got := BigToCompact(n); got != bits {
			t.Fatalf("round-trip %x → %x", bits, got)
		}
	}
	if _, err := CompactToBig(0x00800000); err == nil {
		t.Fatal("a negative target has to be refused")
	}
	if _, err := CompactToBig(0); err == nil {
		t.Fatal("a zero target has to be refused")
	}
}
