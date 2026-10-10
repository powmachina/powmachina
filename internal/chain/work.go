package chain

import (
	"errors"
	"math/big"
)

// Chainwork across the lanes.
//
// The naive 2^256/target does not work: the lanes' difficulties are not on the same scale and an
// attacker would pick the one where work is "bought" cheapest. So work is normalized so that a
// block from every lane weighs the same — by the geometric mean of the average works (the DigiByte
// MultiShield model), recomputed once per epoch.
//
// There are two lanes, so the geometric mean is an integer **square** root.
//
// The whole computation is integer and deterministic. No floats in consensus.

var ErrNoAverage = errors.New("chain: the lane's average work is zero")

// GeoMean2 is the integer geometric mean of two values.
//
// `big.Int.Sqrt` rounds down and is deterministic on every platform, so no hand-written Newton is
// needed here — less code, fewer places to get it wrong.
func GeoMean2(a, b *big.Int) *big.Int {
	prod := new(big.Int).Mul(a, b)
	return new(big.Int).Sqrt(prod)
}

// NormalizedWork recomputes a block's work so that a block from every lane weighs the same:
//
//	W = (2^256 / target) × geomean(avg_C, avg_G) / avg_lane
//
// laneAvg is the average work of this lane's blocks over the last window, geo is the geometric
// mean of both lanes' averages. Both are recomputed once per epoch and derivable from the chain.
func NormalizedWork(bits uint32, laneAvg, geo *big.Int) (*big.Int, error) {
	if laneAvg == nil || laneAvg.Sign() == 0 {
		return nil, ErrNoAverage
	}
	w, err := Work(bits)
	if err != nil {
		return nil, err
	}
	out := new(big.Int).Mul(w, geo)
	return out.Div(out, laneAvg), nil
}
