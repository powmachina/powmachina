package chain

import (
	"math/big"
	"testing"
)

func work(mult float64, base int64) *big.Int {
	r := new(big.Rat).SetFloat64(mult)
	r.Mul(r, new(big.Rat).SetInt64(base))
	out := new(big.Int).Quo(r.Num(), r.Denom())
	return out
}

// The curve has to match what is in the security notes §3.
func TestMessCurve(t *testing.T) {
	cases := []struct {
		age  int64
		want float64
	}{
		{0, 1.0}, {120, 1.0}, {1800, 1.4}, {3600, 2.7}, {7200, 7.0}, {14400, 19.3},
		{messXCap, 31.0}, {10 * messXCap, 31.0},
	}
	for _, c := range cases {
		got := MessMultiplier(c.age)
		if diff := got - c.want; diff > 0.15 || diff < -0.15 {
			t.Errorf("age %d s: multiple %.2f, expected ~%.2f", c.age, got, c.want)
		}
	}
	// The curve has to grow monotonically and have a cap.
	prev := 0.0
	for age := int64(0); age <= 2*messXCap; age += 600 {
		got := MessMultiplier(age)
		if got < prev-1e-9 {
			t.Fatalf("the multiple dropped at age %d s: %.3f < %.3f", age, got, prev)
		}
		if got > 31.5 {
			t.Fatalf("the multiple exceeded the cap at age %d s: %.3f", age, got)
		}
		prev = got
	}
}

// Ordinary operation: a shallow reorg with more work has to pass.
func TestMessAcceptsShallowReorg(t *testing.T) {
	local := big.NewInt(1_000_000)
	if !AcceptsReorg(local, work(1.01, 1_000_000), 120) {
		t.Fatal("a one-block reorg with higher work has to pass")
	}
	if AcceptsReorg(local, local, 0) {
		t.Fatal("with equal work the chain we already have should win")
	}
	if AcceptsReorg(local, big.NewInt(999_999), 0) {
		t.Fatal("a weaker branch must not pass")
	}
}

// A deep reorg has to be refused until it has several times more work.
func TestMessRejectsDeepReorg(t *testing.T) {
	local := big.NewInt(1_000_000)

	if AcceptsReorg(local, work(2.0, 1_000_000), 3600) {
		t.Fatal("an hour-old reorg with 2× the work has to be refused (2.7× is needed)")
	}
	if !AcceptsReorg(local, work(3.0, 1_000_000), 3600) {
		t.Fatal("an hour-old reorg with 3× the work should pass")
	}
	if AcceptsReorg(local, work(30.0, 1_000_000), 8*3600) {
		t.Fatal("an eight-hour reorg with 30× the work has to be refused (the cap is 31×)")
	}
	if !AcceptsReorg(local, work(32.0, 1_000_000), 8*3600) {
		t.Fatal("an eight-hour reorg with 32× the work should pass")
	}
}

func TestMessHandlesNil(t *testing.T) {
	if AcceptsReorg(nil, big.NewInt(1), 0) || AcceptsReorg(big.NewInt(1), nil, 0) {
		t.Fatal("nil input must not lead to accepting a branch")
	}
}
