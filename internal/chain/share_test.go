package chain

import (
	"errors"
	"math/big"
	"testing"

	"powmachina/internal/mh"
)

// fixedHash is a PoWHasher that answers every header with one hash, so a test picks how much work
// a share has.
type fixedHash [32]byte

func (f fixedHash) PoWHash(*Header) ([32]byte, error) { return f, nil }

// hashBelow is the hash `n` below a bound: 2^256 cannot be written in 32 bytes, so the bound is
// handed over as a big.Int.
func hashBelow(t *testing.T, bound *big.Int, n int64) fixedHash {
	t.Helper()
	v := new(big.Int).Sub(bound, big.NewInt(n))
	if v.Sign() < 0 || v.BitLen() > 256 {
		t.Fatalf("no 256-bit hash %d below %x", n, bound)
	}
	var out fixedHash
	v.FillBytes(out[:])
	return out
}

// shareBound is the share's bound computed the long way: (target + 1) × 3000 / 2^weight.
func shareBound(t *testing.T, bits uint32, weight uint8) *big.Int {
	t.Helper()
	target, err := CompactToBig(bits)
	if err != nil {
		t.Fatal(err)
	}
	b := new(big.Int).Add(target, big.NewInt(1))
	b.Mul(b, big.NewInt(SharesPerLaneBlock))
	return b.Rsh(b, uint(weight))
}

// A share built on the tip of a chain whose difficulty is far from the floor, so its bound fits in
// 256 bits and can be hit exactly.
func shareOnTip(t *testing.T) (*HeaderChain, []*Header, *Share) {
	t.Helper()
	params := RegTest
	params.PowLimitBits = MaxTargetBits
	hc, hs := mainChain(t, params, 40)
	h := sideHeader(t, hc, hs[len(hs)-1])
	s := &Share{Header: *h, BodyRoot: [32]byte{7}}
	s.Commit.Payout = [32]byte{1}
	s.Commit.Weight = 3
	s.Header.MerkleRoot = ShareRoot(s.BodyRoot, &s.Commit)
	return hc, hs, s
}

// Everything the share claims is under its PoW: change any field and the merkle root no longer
// matches.
func TestShareRootCoversEveryField(t *testing.T) {
	base := ShareCommit{Payout: [32]byte{1}, FinalityKey: [32]byte{2}, Threshold: 3, Weight: 4}
	body := [32]byte{5}
	root := ShareRoot(body, &base)
	changes := map[string]func(*ShareCommit){
		"payout":       func(c *ShareCommit) { c.Payout[31] ^= 1 },
		"finality key": func(c *ShareCommit) { c.FinalityKey[0] ^= 1 },
		"threshold":    func(c *ShareCommit) { c.Threshold++ },
		"weight":       func(c *ShareCommit) { c.Weight-- },
	}
	for name, change := range changes {
		c := base
		change(&c)
		if ShareRoot(body, &c) == root {
			t.Errorf("the %s is not under the root", name)
		}
	}
	if ShareRoot([32]byte{6}, &base) == root {
		t.Error("the body root is not under the root")
	}
	// The block's root of the same parts is the share's: a block is its closing share.
	b := &Block{Header: &Header{}, Share: base, Txs: []*Tx{coinbaseTx(1, 0, RegTest)}}
	b.Seal()
	if b.Header.MerkleRoot != ShareRoot(BodyRoot(b.Txs, &b.Settlement), &base) || b.CheckRoot() != nil {
		t.Fatal("a sealed block's root is not the share root of its body")
	}
}

// The bound is exact: a hash one below it is a share, the bound itself is not, and each step of
// weight halves it.
func TestShareTargetBound(t *testing.T) {
	const bits = 0x1d00ffff
	for w := uint8(0); w <= MaxShareWeight; w++ {
		bound := shareBound(t, bits, w)
		if ok, err := MeetsShareTarget(hashBelow(t, bound, 1), bits, w); err != nil || !ok {
			t.Fatalf("weight %d: one below the bound is not a share (%v)", w, err)
		}
		if ok, _ := MeetsShareTarget(hashBelow(t, bound, 0), bits, w); ok {
			t.Fatalf("weight %d: the bound itself is a share", w)
		}
	}
	// A block's hash makes a share of every weight: 2^11 < 3000.
	target, _ := CompactToTarget(bits)
	for w := uint8(0); w <= MaxShareWeight; w++ {
		if ok, _ := MeetsShareTarget(target, bits, w); !ok {
			t.Fatalf("a block's hash is not a share of weight %d", w)
		}
	}
	if _, err := MeetsShareTarget(target, bits, MaxShareWeight+1); !errors.Is(err, ErrShareWeight) {
		t.Fatalf("weight %d: %v, expected ErrShareWeight", MaxShareWeight+1, err)
	}
}

func TestShareRoundTrip(t *testing.T) {
	_, _, s := shareOnTip(t)
	s.Commit.FinalityKey = [32]byte{9}
	s.Commit.Threshold = 2
	raw := s.Serialize()
	got, err := DeserializeShare(raw[:])
	if err != nil {
		t.Fatal(err)
	}
	if *got != *s {
		t.Fatal("the share changed on the way")
	}
	if _, err := DeserializeShare(raw[:ShareBytes-1]); err == nil {
		t.Fatal("a short share was read")
	}
}

func TestCheckShare(t *testing.T) {
	hc, hs, s := shareOnTip(t)
	bound := shareBound(t, s.Header.Bits, s.Commit.Weight)
	good, short := hashBelow(t, bound, 1), hashBelow(t, bound, 0)

	if _, err := hc.CheckShare(s, good); err != nil {
		t.Fatalf("a valid share: %v", err)
	}
	if _, err := hc.CheckShare(s, short); !errors.Is(err, ErrSharePoW) {
		t.Fatalf("a hash at the bound: %v, expected ErrSharePoW", err)
	}

	// A relay that lowers the weight to make the share pass, or redirects the payout, breaks the root.
	for name, change := range map[string]func(*Share){
		"weight": func(c *Share) { c.Commit.Weight-- },
		"payout": func(c *Share) { c.Commit.Payout[0] ^= 1 },
		"body":   func(c *Share) { c.BodyRoot[0] ^= 1 },
	} {
		c := *s
		change(&c)
		if _, err := hc.CheckShare(&c, good); !errors.Is(err, ErrShareRoot) {
			t.Fatalf("a changed %s: %v, expected ErrShareRoot", name, err)
		}
	}

	heavy := *s
	heavy.Commit.Weight = MaxShareWeight + 1
	heavy.Header.MerkleRoot = ShareRoot(heavy.BodyRoot, &heavy.Commit)
	if _, err := hc.CheckShare(&heavy, good); !errors.Is(err, ErrShareWeight) {
		t.Fatalf("weight %d: %v, expected ErrShareWeight", heavy.Commit.Weight, err)
	}

	bits := *s
	bits.Header.Bits = 0x1d00ffff
	if _, err := hc.CheckShare(&bits, good); !errors.Is(err, ErrBadBits) {
		t.Fatalf("bits that are not the lane's: %v, expected ErrBadBits", err)
	}

	early := *s
	early.Header.Time = hs[len(hs)-1].Time - 1
	if _, err := hc.CheckShare(&early, good); !errors.Is(err, ErrTimeTooOld) {
		t.Fatalf("dated before its parent: %v, expected ErrTimeTooOld", err)
	}

	orphan := *s
	orphan.Header.PrevBlock = [32]byte{0xee}
	if _, err := hc.CheckShare(&orphan, good); !errors.Is(err, ErrUnknownParent) {
		t.Fatalf("an unknown parent: %v, expected ErrUnknownParent", err)
	}

	tall := *s
	tall.Header.Height++
	if _, err := hc.CheckShare(&tall, good); !errors.Is(err, ErrBadHeight) {
		t.Fatalf("a height that does not follow the parent: %v, expected ErrBadHeight", err)
	}
}

// A share may build on a block up to ShareExpiry below the next block — ShareExpiry - 1 below the
// tip; one deeper and no block could include it any more.
func TestShareExpiry(t *testing.T) {
	hc, hs, _ := shareOnTip(t)
	tip := len(hs) - 1
	for depth, want := range map[int]error{0: nil, ShareExpiry - 1: nil, ShareExpiry: ErrShareExpired} {
		h := sideHeader(t, hc, hs[tip-depth])
		s := &Share{Header: *h}
		s.Header.MerkleRoot = ShareRoot(s.BodyRoot, &s.Commit)
		bound := shareBound(t, s.Header.Bits, 0)
		if _, err := hc.CheckShare(s, hashBelow(t, bound, 1)); !errors.Is(err, want) {
			t.Fatalf("a share %d below the tip: %v, expected %v", depth, err, want)
		}
	}
}

// A block travels with its share commitment, and a copy with another commitment is another block.
func TestBlockCarriesTheShareCommitment(t *testing.T) {
	_, b := sealedSpend(t, 10, RegTest)
	b.Share = ShareCommit{Payout: [32]byte{1}, FinalityKey: [32]byte{2}, Threshold: 3, Weight: 4}
	b.Seal()
	c := copyOf(t, b)
	if c.Share != b.Share || c.Header.ID() != b.Header.ID() || c.CheckRoot() != nil {
		t.Fatal("the commitment did not survive the round trip")
	}
	c.Share.Threshold = 0
	if err := c.CheckRoot(); !errors.Is(err, ErrBadMerkle) {
		t.Fatalf("a copy with another threshold: %v, expected ErrBadMerkle", err)
	}
	c.Share = b.Share
	c.Share.Weight = MaxShareWeight + 1
	if err := c.CheckRoot(); !errors.Is(err, ErrShareWeight) {
		t.Fatalf("a block of weight %d: %v, expected ErrShareWeight", c.Share.Weight, err)
	}
}

// The share's hash is MachinaHash itself: whatever meets a block's target through CheckPoW meets
// every share target through PoWHash, and shares come as often as the bound says.
func TestPoWHashIsTheBlocksHash(t *testing.T) {
	v := mh.NewVerifier(0, mh.TestProfile)
	defer v.Close()
	hasher := mh.NewHasher()
	defer hasher.Close()
	pair := PoWPair{Hasher: hasher, Verifier: v}

	h := sampleHeader()
	h.Height = 7
	h.Lane = mh.LaneG
	h.Bits = MaxTargetBits // ≈ 65 536 hashes a block, ≈ 22 a minimum share
	const tries = 10_000
	target, err := CompactToTarget(h.Bits)
	if err != nil {
		t.Fatal(err)
	}
	shares := 0
	for nonce := uint64(0); nonce < tries; nonce++ {
		h.Nonce = nonce
		hash, err := pair.PoWHash(h)
		if err != nil {
			t.Fatal(err)
		}
		share, err := MeetsShareTarget(hash, h.Bits, 0)
		if err != nil {
			t.Fatal(err)
		}
		block := mh.MeetsTarget(&hash, &target)
		if share {
			// Only here, the hash is the expensive part: CheckPoW has to agree with PoWHash.
			if ok, err := h.CheckPoW(hasher, v); err != nil || ok != block {
				t.Fatalf("nonce %d: CheckPoW says %v, the share's hash says %v (%v)", nonce, ok, block, err)
			}
		}
		if block {
			for w := uint8(0); w <= MaxShareWeight; w++ {
				if ok, _ := MeetsShareTarget(hash, h.Bits, w); !ok {
					t.Fatalf("nonce %d meets the block's target and is not a share of weight %d", nonce, w)
				}
			}
		}
		if share {
			shares++
		}
	}
	// The chance of a share is bound / 2^256; the count is binomial, its spread ~21 here.
	p, _ := new(big.Float).Quo(new(big.Float).SetInt(shareBound(t, h.Bits, 0)),
		new(big.Float).SetInt(new(big.Int).Lsh(big.NewInt(1), 256))).Float64()
	want := p * tries
	if float64(shares) < want*0.85 || float64(shares) > want*1.15 {
		t.Fatalf("%d shares in %d hashes, expected about %.0f", shares, tries, want)
	}
	t.Logf("%d shares in %d hashes, expected %.0f", shares, tries, want)
}

// A share on a block that was found invalid votes for nothing, whether the mark came from
// validation or from storage after the headers were loaded.
func TestShareOnAnInvalidBlockIsRefused(t *testing.T) {
	for _, loaded := range []bool{false, true} {
		hc, hs, _ := shareOnTip(t)
		bad := hs[len(hs)-3]
		if loaded {
			hc.MarkInvalidLoaded(bad.ID())
		} else if _, _, err := hc.Invalidate(bad.ID()); err != nil {
			t.Fatal(err)
		}
		if !hc.IsInvalid(hs[len(hs)-1].ID()) {
			t.Fatalf("loaded=%v: a block above the mark is not invalid", loaded)
		}
		// The tip moved below the mark, so the share is built on what is left of the old branch.
		h := sideHeader(t, hc, hs[len(hs)-2])
		s := &Share{Header: *h}
		s.Header.MerkleRoot = ShareRoot(s.BodyRoot, &s.Commit)
		bound := shareBound(t, s.Header.Bits, 0)
		if _, err := hc.CheckShare(s, hashBelow(t, bound, 1)); !errors.Is(err, ErrInvalidAncestor) {
			t.Fatalf("loaded=%v: a share above an invalid block: %v, expected ErrInvalidAncestor", loaded, err)
		}
	}
}
