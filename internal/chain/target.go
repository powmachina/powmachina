package chain

import (
	"errors"
	"math/big"
)

// Bits is the target encoded into 32 bits (an exponent + a mantissa), the same principle as in
// Bitcoin. The compact form keeps the header small and makes comparing targets cheap.

var (
	// MaxTargetBits is the lowest allowed mainnet difficulty (the floor for a lane that died out).
	// The target is ≈ 2^240, so ≈ 65 thousand attempts per block — one ordinary CPU keeps a lane alive.
	MaxTargetBits uint32 = 0x1f00ffff

	// RegtestTargetBits is the trivial target for regtest and the selfcheck (≈ 16 attempts).
	//
	// Why so low: on regtest lane C mines too, and that is RandomX in light mode — one hash there
	// costs ~20 ms, so with 256 attempts **one block took 5–15 seconds** and the chain sync tests
	// were measured in minutes. With 16 attempts it is tens of milliseconds and regtest does what it
	// is for: it runs, nobody waits for it.
	RegtestTargetBits uint32 = 0x200fffff

	ErrBadCompact = errors.New("chain: invalid compact target")
	ErrTargetZero = errors.New("chain: zero target")
)

// two256 is 2^256, the denominator when converting a target into work.
var two256 = new(big.Int).Lsh(big.NewInt(1), 256)

// CompactToBig unpacks a compact target.
func CompactToBig(bits uint32) (*big.Int, error) {
	exponent := bits >> 24
	mantissa := bits & 0x007fffff
	if bits&0x00800000 != 0 {
		return nil, ErrBadCompact // negative targets make no sense
	}
	out := new(big.Int).SetUint64(uint64(mantissa))
	if exponent <= 3 {
		out.Rsh(out, uint(8*(3-exponent)))
	} else {
		out.Lsh(out, uint(8*(exponent-3)))
	}
	if out.Sign() == 0 {
		return nil, ErrTargetZero
	}
	if out.BitLen() > 256 {
		return nil, ErrBadCompact
	}
	return out, nil
}

// BigToCompact packs a target. It always rounds up (a looser target), so difficulty does not creep
// upwards on its own through recomputation.
func BigToCompact(target *big.Int) uint32 {
	if target.Sign() <= 0 {
		return 0
	}
	exponent := uint32(len(target.Bytes()))
	var mantissa uint32
	if exponent <= 3 {
		mantissa = uint32(target.Uint64()) << (8 * (3 - exponent))
	} else {
		shifted := new(big.Int).Rsh(target, uint(8*(exponent-3)))
		mantissa = uint32(shifted.Uint64())
	}
	if mantissa&0x00800000 != 0 {
		mantissa >>= 8
		exponent++
	}
	return exponent<<24 | mantissa
}

// CompactToTarget returns the target as 32 B big-endian for comparison with a PoW result.
func CompactToTarget(bits uint32) ([32]byte, error) {
	var out [32]byte
	n, err := CompactToBig(bits)
	if err != nil {
		return out, err
	}
	b := n.Bytes()
	copy(out[32-len(b):], b)
	return out, nil
}

// Work is the work corresponding to a target: 2^256 / (target + 1).
func Work(bits uint32) (*big.Int, error) {
	target, err := CompactToBig(bits)
	if err != nil {
		return nil, err
	}
	denom := new(big.Int).Add(target, big.NewInt(1))
	return new(big.Int).Div(two256, denom), nil
}
