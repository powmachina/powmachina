package chain

import (
	"math/big"
	"time"

	"powmachina/internal/mh"
)

// The lane retarget from ForkLaneDifficulty on (SPEC §3.6).
//
// A lane's target is its last block's target moved by how far the lane is from its pace of one
// block per LaneBlockTime: it doubles for every AsertHalfLife the lane is behind and halves for
// every one it is ahead. This is ASERT (aserti3-2d, Bitcoin Cash 2020) in its relative form, with
// two things that come from having two lanes:
//
//   - The clock is the tip's. The time a lane is measured over runs from the block its last block
//     was built on to the block the next one is built on, whichever lanes those are. A lane that
//     fell silent therefore gets easier at the speed of real time — the other lane keeps the clock
//     moving — and not at the speed of its own blocks, which is what made the windowed retarget
//     take half a day to notice that nine tenths of a lane had left.
//   - A block let in by the rotation relief had to wait LaneRotationRelief whatever its lane's
//     hashrate was. That wait is not counted. Without this a lane mining alone, one block per
//     relief, reads as a lane that lost its hashrate and slides to the difficulty floor.
//
// The relative form matters at the floor: the lane's debt is forgotten there. Counting from a
// fixed anchor instead, a lane that was gone for days would have to mine every block it missed
// before its difficulty moved at all.

const (
	// AsertHalfLife: a lane this far behind its pace gets twice the target. One hour was picked
	// by simulation (cmd/powm-diffsim): half of it swings the difficulty by a fifth on a chain at
	// rest, twice of it takes six hours to follow a lane that lost 90 %.
	AsertHalfLife = time.Hour
	// AsertMaxHalvings bounds how far one gap can move a lane, either way: 2^4 = 16×. A lane that
	// was away for days comes back sixteen times easier than it left and not on the floor, where
	// it would be nearly free for the day it takes its own blocks to climb back. A lane that
	// really lost more than that gets there in the next gap.
	AsertMaxHalvings = 4

	asertRadix = 16 // the exponent is a fixed-point number with this many fractional bits
)

// AsertNext returns the target of a lane's next block: `bits` is the target of its last block
// and `behind` is, in seconds, how much longer than LaneBlockTime the lane has been waiting
// (negative when it is ahead).
func AsertNext(bits uint32, behind int64, powLimit uint32) (uint32, error) {
	target, err := CompactToBig(bits)
	if err != nil {
		return 0, err
	}
	maxTarget, err := CompactToBig(powLimit)
	if err != nil {
		return 0, err
	}

	halfLife := int64(AsertHalfLife / time.Second)
	if limit := AsertMaxHalvings * halfLife; behind > limit {
		behind = limit
	} else if behind < -limit {
		behind = -limit
	}

	// exponent = behind / halfLife in 16.16 fixed point, rounded towards minus infinity so the
	// fractional part below is never negative.
	exponent := behind << asertRadix
	q := exponent / halfLife
	if exponent%halfLife < 0 {
		q--
	}
	shifts := q >> asertRadix
	frac := uint64(q & (1<<asertRadix - 1))

	// 2^frac for 0 <= frac < 1 as a cubic, in 16.16 — the polynomial of aserti3-2d, off by less
	// than 0.013 %. It is computed in big.Int: the sum fits 64 bits only by a hair.
	f := new(big.Int).SetUint64(frac)
	poly := new(big.Int).Mul(big.NewInt(195766423245049), f)
	f2 := new(big.Int).Mul(f, f)
	poly.Add(poly, new(big.Int).Mul(big.NewInt(971821376), f2))
	poly.Add(poly, new(big.Int).Mul(big.NewInt(5127), new(big.Int).Mul(f2, f)))
	poly.Add(poly, new(big.Int).Lsh(big.NewInt(1), 47))
	poly.Rsh(poly, 48)
	factor := poly.Add(poly, big.NewInt(1<<asertRadix))

	next := new(big.Int).Mul(target, factor)
	if shifts < 0 {
		next.Rsh(next, uint(-shifts))
	} else {
		next.Lsh(next, uint(shifts))
	}
	next.Rsh(next, asertRadix)

	if next.Sign() == 0 {
		next.SetInt64(1)
	}
	if next.Cmp(maxTarget) > 0 {
		next.Set(maxTarget)
	}
	return clampBitsToFloor(BigToCompact(next), powLimit), nil
}

// asertBitsLocked is the target of a lane's next block on top of `parent` under
// ForkLaneDifficulty.
func (hc *HeaderChain) asertBitsLocked(parent *node, lane mh.Lane) (uint32, error) {
	genesis := hc.byID[hc.genesis]
	if parent == genesis {
		return genesis.header.Bits, nil
	}
	// The genesis is outside time: it is written long before the first block is mined, so the
	// network's clock starts at the first block. A lane that has no block yet counts from there
	// with the genesis difficulty — if that was set too high for it, it comes down by itself.
	bits, since, relief := genesis.header.Bits, parent.first.header.Time, false
	if last := parent.lastOf[lane]; last != nil {
		bits = last.header.Bits
		before := hc.byID[last.header.PrevBlock]
		if before == genesis {
			since = last.header.Time
		} else {
			since = before.header.Time
		}
		relief = !LaneAllowed(hc.recentLanesLocked(before, MaxSameLaneRun), lane)
	}
	behind := int64(parent.header.Time) - int64(since) - int64(LaneBlockTime/time.Second)
	if relief {
		behind -= int64(LaneRotationRelief / time.Second)
	}
	return AsertNext(bits, behind, hc.params.PowLimitBits)
}
