package chain

import (
	"errors"
	"math/big"
)

// LWMA-1 (zawy12) computed over the blocks of **the same lane**. Every lane has its own retarget
// and its own history; the target time is LaneBlockTime (240 s), so two lanes give a 120 s block.

var ErrShortWindow = errors.New("chain: too few blocks for a retarget")

// LaneBlock is what the retarget needs from a previous block of a given lane.
type LaneBlock struct {
	Time uint32
	Bits uint32
}

const (
	// maxSolvetimeFactor clips one extreme solvetime so it does not outweigh the whole window.
	maxSolvetimeFactor = 6
	// maxAdjustFactor is the cap on a difficulty jump per window (×4 / ÷4).
	maxAdjustFactor = 4
)

// NextBits returns the target for the next block of a given lane.
//
// window are this lane's most recent blocks ordered from the oldest to the newest; at least
// LwmaWindow+1 of them are needed (the first only serves to compute the first solvetime). With a
// shorter history (the start of the chain) it returns the last block's bits. powLimit is the given
// network's difficulty floor.
func NextBits(window []LaneBlock, powLimit uint32) (uint32, error) {
	if len(window) < 2 {
		return 0, ErrShortWindow
	}
	if len(window) < LwmaWindow+1 {
		// A short history (the start of the chain). The floor has to be enforced here too — the bits
		// come from headers sent by foreign peers and must not be repeated blindly.
		return clampBitsToFloor(window[len(window)-1].Bits, powLimit), nil
	}
	window = window[len(window)-(LwmaWindow+1):]

	targetSpacing := int64(LaneBlockTime.Seconds())
	maxSolvetime := targetSpacing * maxSolvetimeFactor

	weightedSolvetime := big.NewInt(0)
	sumTargets := big.NewInt(0)
	for i := 1; i <= LwmaWindow; i++ {
		st := int64(window[i].Time) - int64(window[i-1].Time)
		// Timestamps can go backwards; clipping keeps the retarget stable.
		if st < 1 {
			st = 1
		}
		if st > maxSolvetime {
			st = maxSolvetime
		}
		weightedSolvetime.Add(weightedSolvetime, big.NewInt(st*int64(i)))

		t, err := CompactToBig(window[i].Bits)
		if err != nil {
			return 0, err
		}
		sumTargets.Add(sumTargets, t)
	}

	// k = N(N+1)/2 × T — the value weightedSolvetime should have at exactly the target pace.
	k := big.NewInt(int64(LwmaWindow) * int64(LwmaWindow+1) / 2 * targetSpacing)

	// next = (sumTargets / N) × weightedSolvetime / k
	next := new(big.Int).Mul(sumTargets, weightedSolvetime)
	next.Div(next, big.NewInt(int64(LwmaWindow)))
	next.Div(next, k)

	avgTarget := new(big.Int).Div(sumTargets, big.NewInt(int64(LwmaWindow)))
	next = clampAdjust(next, avgTarget)

	maxTarget, err := CompactToBig(powLimit)
	if err != nil {
		return 0, err
	}
	if next.Cmp(maxTarget) > 0 {
		// The difficulty floor: a dead lane has to get back within reach of an ordinary miner quickly.
		next.Set(maxTarget)
	}
	if next.Sign() <= 0 {
		return 0, ErrTargetZero
	}

	// BigToCompact rounds up, so clipping before encoding is not enough — the rounding can push the
	// result just above the floor.
	return clampBitsToFloor(BigToCompact(next), powLimit), nil
}

// clampBitsToFloor is the only place the difficulty floor is enforced. It returns the input if it
// is valid and below the floor, otherwise the floor itself.
func clampBitsToFloor(bits, powLimit uint32) uint32 {
	target, err := CompactToBig(bits)
	if err != nil {
		return powLimit
	}
	maxTarget, err := CompactToBig(powLimit)
	if err != nil || target.Cmp(maxTarget) > 0 {
		return powLimit
	}
	return bits
}

// clampAdjust keeps the change of target within ×4 / ÷4 of the window's average.
func clampAdjust(next, avg *big.Int) *big.Int {
	upper := new(big.Int).Mul(avg, big.NewInt(maxAdjustFactor))
	lower := new(big.Int).Div(avg, big.NewInt(maxAdjustFactor))
	if next.Cmp(upper) > 0 {
		return upper
	}
	if next.Cmp(lower) < 0 {
		return lower
	}
	return next
}
