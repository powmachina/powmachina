package chain

import (
	"errors"
	"fmt"
	"math/big"

	"powmachina/internal/mh"
)

// The Sharepool's share (the Sharepool spec §2).
//
// A share is a block header whose hash meets an easier target than the block's. Everything a
// share claims is under its PoW, so nobody who relays it can change it:
//
//	merkle_root = BLAKE3("PowMachina/share/v1" ‖ body_root ‖ payout ‖ finality_key ‖ threshold ‖ weight)
//
// `body_root` commits to the block the miner would close. A share whose hash also meets the
// block's target is that block: the block carries the same commitment, and its body has to hash to
// the same `body_root`. Nobody can attach another body, or another payout, to a lucky share.
//
// The header's `bits` are the lane's block target, the same number a block on the same parent has
// to carry. The share's own target is derived from it: a share of weight k is worth 2^k minimum
// shares and needs a hash 2^k times lower. With the bits checked like a block's, the retarget, the
// chainwork and the block rules stay as they are; the share only adds the weight.

const (
	// SharesPerLaneBlock is how many minimum shares a lane finds per block of its own: 100 shares a
	// second across the network, 50 per lane, a lane block every 60 s (the Sharepool spec §2.2).
	SharesPerLaneBlock = 3000
	// MaxShareWeight is the heaviest share: 2^11 = 2048 minimum shares, still under a block's 3000,
	// so a block always meets the target of its own weight.
	MaxShareWeight = 11
	// ShareExpiry is how far below the block that includes it a share's parent may be: 1 is the
	// block's parent, 8 the deepest (the Sharepool spec §3.1). A share that the next block could no
	// longer include is not relayed either.
	ShareExpiry = 8
	// ShareCommitBytes is the commitment a block and a share carry next to the header.
	ShareCommitBytes = 32 + 32 + 1 + 1
	// ShareBytes is a share in full: the header, the body root and the commitment. On the wire it
	// travels shorter (the Sharepool spec §2.3); this is the form it is verified in.
	ShareBytes = HeaderBytes + 32 + ShareCommitBytes

	shareRootTag = "PowMachina/share/v1"
)

var (
	ErrShareWeight  = errors.New("chain: the share weight is above the maximum")
	ErrShareRoot    = errors.New("chain: the merkle root does not commit to the share")
	ErrShareExpired = errors.New("chain: the share builds on a block too far below the tip")
	ErrSharePoW     = errors.New("chain: the share does not meet its target")
)

// ShareCommit is what the miner of a share or a block commits to besides the block's body.
type ShareCommit struct {
	// Payout is the program of a version 0 address the share is credited to.
	Payout [32]byte
	// FinalityKey is the miner's finality key (the finality spec); all zeros = none.
	FinalityKey [32]byte
	// Threshold is the payout threshold the miner asks for (the Sharepool spec §5.2).
	Threshold uint8
	// Weight: the share is worth 2^Weight minimum shares.
	Weight uint8
}

// Serialize writes the commitment as it travels and as it is hashed.
func (c *ShareCommit) Serialize() [ShareCommitBytes]byte {
	var out [ShareCommitBytes]byte
	copy(out[0:32], c.Payout[:])
	copy(out[32:64], c.FinalityKey[:])
	out[64] = c.Threshold
	out[65] = c.Weight
	return out
}

// DeserializeShareCommit reads a commitment.
func DeserializeShareCommit(b []byte) (ShareCommit, error) {
	var c ShareCommit
	if len(b) < ShareCommitBytes {
		return c, errShort
	}
	copy(c.Payout[:], b[0:32])
	copy(c.FinalityKey[:], b[32:64])
	c.Threshold = b[64]
	c.Weight = b[65]
	return c, nil
}

// Units is what the share counts for: 2^Weight minimum shares.
func (c *ShareCommit) Units() uint64 { return 1 << c.Weight }

// HasFinalityKey says whether the miner gave a finality key.
func (c *ShareCommit) HasFinalityKey() bool { return c.FinalityKey != [32]byte{} }

// ShareRoot is the header's merkle root for a body root and a commitment.
func ShareRoot(bodyRoot [32]byte, c *ShareCommit) [32]byte {
	commit := c.Serialize()
	var buf [32 + ShareCommitBytes]byte
	copy(buf[:32], bodyRoot[:])
	copy(buf[32:], commit[:])
	return mh.TaggedHash(shareRootTag, buf[:])
}

// MeetsShareTarget says whether a PoW hash makes a share of the given weight under the lane's
// block target `bits`.
//
// A minimum share has 1/3000 of a block's work. Work is 2^256 / (target + 1), so the bound is
//
//	hash < (target + 1) × 3000 / 2^weight
//
// in integers, the division rounding down. A bound above 2^256 lets every hash through, which only
// happens near the network's floor.
func MeetsShareTarget(hash [32]byte, bits uint32, weight uint8) (bool, error) {
	if weight > MaxShareWeight {
		return false, ErrShareWeight
	}
	target, err := CompactToBig(bits)
	if err != nil {
		return false, err
	}
	bound := new(big.Int).Add(target, big.NewInt(1))
	bound.Mul(bound, big.NewInt(SharesPerLaneBlock))
	bound.Rsh(bound, uint(weight))
	return new(big.Int).SetBytes(hash[:]).Cmp(bound) < 0, nil
}

// ShareTarget is MeetsShareTarget as a target for mh.MeetsTarget: the highest hash a share of
// `weight` on a block of `bits` may have. It is never above the largest hash there is.
func ShareTarget(bits uint32, weight uint8) ([32]byte, error) {
	var out [32]byte
	if weight > MaxShareWeight {
		return out, ErrShareWeight
	}
	target, err := CompactToBig(bits)
	if err != nil {
		return out, err
	}
	bound := new(big.Int).Add(target, big.NewInt(1))
	bound.Mul(bound, big.NewInt(SharesPerLaneBlock))
	bound.Rsh(bound, uint(weight))
	bound.Sub(bound, big.NewInt(1))
	if bound.BitLen() > 256 {
		for i := range out {
			out[i] = 0xff
		}
		return out, nil
	}
	bound.FillBytes(out[:])
	return out, nil
}

// Share is a share in the form it is verified in.
type Share struct {
	Header   Header
	BodyRoot [32]byte
	Commit   ShareCommit
}

// Serialize writes the share in full.
func (s *Share) Serialize() [ShareBytes]byte {
	var out [ShareBytes]byte
	h := s.Header.Serialize()
	copy(out[:HeaderBytes], h[:])
	copy(out[HeaderBytes:HeaderBytes+32], s.BodyRoot[:])
	c := s.Commit.Serialize()
	copy(out[HeaderBytes+32:], c[:])
	return out
}

// DeserializeShare reads a share in full from untrusted bytes.
func DeserializeShare(b []byte) (*Share, error) {
	if len(b) != ShareBytes {
		return nil, fmt.Errorf("chain: a share is %d B, got %d", ShareBytes, len(b))
	}
	h, err := DeserializeHeader(b[:HeaderBytes])
	if err != nil {
		return nil, err
	}
	s := &Share{Header: *h}
	copy(s.BodyRoot[:], b[HeaderBytes:HeaderBytes+32])
	if s.Commit, err = DeserializeShareCommit(b[HeaderBytes+32:]); err != nil {
		return nil, err
	}
	return s, nil
}

// ID is the share's identity: the id of its header, which commits to everything else.
func (s *Share) ID() [32]byte { return s.Header.ID() }

// PoWHasher computes a header's PoW hash. A share needs the value, not a yes or no against the
// header's own bits.
type PoWHasher interface {
	PoWHash(h *Header) ([32]byte, error)
}

// PoWHash implements PoWHasher for one epoch.
func (p PoWPair) PoWHash(h *Header) ([32]byte, error) {
	if want := EpochForHeight(uint64(h.Height)); p.Verifier.Epoch() != want {
		return [32]byte{}, errors.New("chain: the verifier has a different epoch than the header needs")
	}
	pow := h.PoWInput()
	return p.Hasher.Hash(p.Verifier, &pow, h.Nonce, h.Lane)
}

// CheckShare verifies a share against the chain: everything a block on the same parent would have
// to meet in its header, except the lane rotation and the PoW, which is the share's own.
//
// The lane rotation is a rule about which blocks follow each other; a share of a lane that may not
// close the next block is still work on the parent, and it is paid like any other.
//
// The cheap checks run under the lock, the PoW after it: shares come a hundred a second. It
// returns the PoW hash, so whoever keeps the share does not compute it again when a block
// settles it.
func (hc *HeaderChain) CheckShare(s *Share, pow PoWHasher) ([32]byte, error) {
	h := &s.Header
	var none [32]byte
	if s.Commit.Weight > MaxShareWeight {
		return none, ErrShareWeight
	}
	if h.MerkleRoot != ShareRoot(s.BodyRoot, &s.Commit) {
		return none, ErrShareRoot
	}
	if h.Lane > mh.LaneG {
		return none, ErrBadLane
	}
	if err := hc.checkShareLocked(h); err != nil {
		return none, err
	}
	hash, err := pow.PoWHash(h)
	if err != nil {
		return none, err
	}
	ok, err := MeetsShareTarget(hash, h.Bits, s.Commit.Weight)
	if err != nil {
		return none, err
	}
	if !ok {
		return none, ErrSharePoW
	}
	return hash, nil
}

func (hc *HeaderChain) checkShareLocked(h *Header) error {
	hc.mu.Lock()
	defer hc.mu.Unlock()
	parent, ok := hc.byID[h.PrevBlock]
	if !ok {
		return ErrUnknownParent
	}
	if uint64(h.Height) != parent.height+1 {
		return ErrBadHeight
	}
	// The next block is tip + 1; the share's parent has to be at most ShareExpiry below it.
	if parent.height+ShareExpiry <= hc.tip.height {
		return fmt.Errorf("%w: it builds on %d, the tip is %d", ErrShareExpired, parent.height, hc.tip.height)
	}
	// The parent's branch has to meet the active one within the expiry: a share on a branch that
	// forked deeper could be included by nothing.
	if hc.forkPointBoundedLocked(parent, ShareExpiry) == nil {
		return fmt.Errorf("%w: its branch forks too deep", ErrShareExpired)
	}
	if parent.tainted {
		return ErrInvalidAncestor
	}
	if h.Time < hc.minTimeLocked(parent) {
		return ErrTimeTooOld
	}
	if int64(h.Time) > hc.now().Add(hc.params.FutureDrift(uint64(h.Height))).Unix() {
		return ErrTimeTooNew
	}
	wantBits, err := hc.nextBitsLocked(parent, h.Lane)
	if err != nil {
		return err
	}
	if h.Bits != wantBits {
		return fmt.Errorf("%w: got %08x, expected %08x", ErrBadBits, h.Bits, wantBits)
	}
	return nil
}
