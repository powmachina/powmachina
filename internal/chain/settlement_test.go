package chain

import (
	"errors"
	"testing"

	"powmachina/internal/mh"
)

// lowHash is a PoWHasher whose every hash is a share of any weight and no block.
var lowHash = fixedHash{0, 0, 0, 1}

// settleChain is a chain of 40 blocks far from the floor and a builder of shares on its blocks:
// share(depth, …) builds on the block `depth` below the next one, as a miner would have mined it.
func settleChain(t testing.TB) (*HeaderChain, []*Header, func(depth int, key byte, lane mh.Lane, nonce uint64) *Share) {
	t.Helper()
	params := RegTest
	params.PowLimitBits = MaxTargetBits
	hc, hs := mainChain(t, params, 40)
	tip := len(hs) - 1
	share := func(depth int, key byte, lane mh.Lane, nonce uint64) *Share {
		parent := hs[tip-depth+1]
		hc.mu.RLock()
		pn := hc.byID[parent.ID()]
		bits, err := hc.nextBitsLocked(pn, lane)
		hc.mu.RUnlock()
		if err != nil {
			t.Fatal(err)
		}
		s := &Share{Header: Header{Version: 1, PrevBlock: parent.ID(), Bits: bits, Height: parent.Height + 1,
			Time: parent.Time + 10, Nonce: nonce, Lane: lane}, BodyRoot: [32]byte{key, 0xb0}}
		s.Commit = ShareCommit{Payout: [32]byte{key}, Threshold: 1, Weight: 2}
		s.Header.MerkleRoot = ShareRoot(s.BodyRoot, &s.Commit)
		return s
	}
	return hc, hs, share
}

// nextBlock is a block on the tip carrying `shares`, sealed.
func nextBlock(t testing.TB, hs []*Header, shares ...*Share) *Block {
	t.Helper()
	tip := hs[len(hs)-1]
	s, err := NewSettlement(shares, uint64(tip.Height)+1)
	if err != nil {
		t.Fatal(err)
	}
	b := &Block{Header: &Header{Version: 1, PrevBlock: tip.ID(), Height: tip.Height + 1, Time: tip.Time + 60},
		Settlement: *s, Txs: []*Tx{coinbaseTx(uint64(tip.Height)+1, 0, RegTest)}}
	b.Seal()
	return b
}

func viewFor(t *testing.T, hc *HeaderChain, b *Block, settled ...[32]byte) *SettlementView {
	t.Helper()
	set := map[[32]byte]bool{}
	for _, id := range settled {
		set[id] = true
	}
	v, err := hc.SettlementViewOn(b.Header.PrevBlock, func(id [32]byte) bool { return set[id] })
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestSettlementRoundTrip(t *testing.T) {
	if got := len((&Settlement{}).Serialize()); got != 6 {
		t.Fatalf("an empty settlement is %d B, expected 6", got)
	}
	_, hs, share := settleChain(t)
	b := nextBlock(t, hs, share(1, 1, mh.LaneC, 1), share(3, 1, mh.LaneC, 2), share(8, 2, mh.LaneG, 3))
	raw := b.Settlement.Serialize()
	got, used, err := DeserializeSettlement(raw)
	if err != nil || used != len(raw) {
		t.Fatalf("read %d of %d: %v", used, len(raw), err)
	}
	if string(got.Serialize()) != string(raw) {
		t.Fatal("the settlement changed on the way")
	}
	// Two miners, two templates, three shares: the tables carry each once.
	if len(got.Keys) != 2 || len(got.Jobs) != 2 || len(got.Shares) != 3 {
		t.Fatalf("%d keys, %d jobs, %d shares", len(got.Keys), len(got.Jobs), len(got.Shares))
	}
	c, err := DeserializeBlock(b.Serialize())
	if err != nil {
		t.Fatal(err)
	}
	if c.CheckRoot() != nil || c.Header.ID() != b.Header.ID() {
		t.Fatal("the block with its settlement did not survive the round trip")
	}
	// The settlement is under the body root: a copy with a share left out is another block.
	c.Settlement.Shares = c.Settlement.Shares[:2]
	if err := c.CheckRoot(); !errors.Is(err, ErrBadMerkle) {
		t.Fatalf("a copy with a share left out: %v, expected ErrBadMerkle", err)
	}
	// A count that promises more than the bytes hold is refused before anything is allocated.
	bad := append([]byte{}, raw...)
	bad[0], bad[1] = 0xff, 0xff
	if _, _, err := DeserializeSettlement(bad); !errors.Is(err, ErrSettlement) {
		t.Fatalf("a count of 65535 keys: %v", err)
	}
}

// The shares a block settles are the ones its miner found, rebuilt exactly: same ids, same keys,
// same weight, and nothing else.
func TestCheckSettlement(t *testing.T) {
	hc, hs, share := settleChain(t)
	shares := []*Share{share(1, 1, mh.LaneC, 1), share(2, 1, mh.LaneG, 2), share(8, 2, mh.LaneG, 3)}
	b := nextBlock(t, hs, shares...)
	got, err := b.CheckSettlement(viewFor(t, hc, b), lowHash, RegTest)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(shares) {
		t.Fatalf("%d settled, expected %d", len(got), len(shares))
	}
	want := map[[32]byte]*Share{}
	for _, s := range shares {
		want[s.ID()] = s
		// Each is also a share the network relays: the same rules from the other side.
		if _, err := hc.CheckShare(s, lowHash); err != nil {
			t.Fatalf("share at height %d as relayed: %v", s.Header.Height, err)
		}
	}
	for _, g := range got {
		s, ok := want[g.ID]
		if !ok {
			t.Fatalf("settled %x, which nobody mined", g.ID[:6])
		}
		if g.Key.Payout != s.Commit.Payout || g.Units != 4 || g.Lane != s.Header.Lane {
			t.Fatalf("share %x settled as %+v", g.ID[:6], g)
		}
	}
	ids, err := b.Settlement.ShareIDs(viewFor(t, hc, b))
	if err != nil || len(ids) != 3 {
		t.Fatalf("ShareIDs: %d (%v)", len(ids), err)
	}
	for i := range ids {
		if ids[i] != got[i].ID {
			t.Fatal("ShareIDs does not give what CheckSettlement settled")
		}
	}
}

func TestSettlementRefuses(t *testing.T) {
	hc, hs, share := settleChain(t)
	a, b2 := share(1, 1, mh.LaneC, 1), share(2, 2, mh.LaneG, 2)

	check := func(name string, b *Block, v *SettlementView, pow PoWHasher, want error) {
		t.Helper()
		if _, err := b.CheckSettlement(v, pow, RegTest); !errors.Is(err, want) {
			t.Fatalf("%s: %v, expected %v", name, err, want)
		}
	}

	blk := nextBlock(t, hs, a, b2)
	check("already settled below", blk, viewFor(t, hc, blk, a.ID()), lowHash, ErrShareSettled)
	check("no PoW", blk, viewFor(t, hc, blk), fixedHash{0xff}, ErrSharePoW)
	// A light node takes the PoW on trust and checks the rest.
	check("no PoW, light", blk, viewFor(t, hc, blk), nil, nil)
	check("settled, light", blk, viewFor(t, hc, blk, a.ID()), nil, ErrShareSettled)

	// A block of the branch is not a share to pay a second time: the view knows the blocks within
	// the expiry by id.
	tip := hs[len(hs)-1]
	v := viewFor(t, hc, blk)
	for d := 1; d <= ShareExpiry; d++ {
		if !v.isBlock(hs[len(hs)-d].ID()) {
			t.Fatalf("the block %d below is not known to the view", d)
		}
	}
	if v.isBlock(a.ID()) {
		t.Fatal("a share is taken for a block")
	}

	late := share(1, 3, mh.LaneC, 4)
	late.Header.Time = tip.Time + 60 + 31 // the block is at tip + 60, the drift 30 s
	late.Header.MerkleRoot = ShareRoot(late.BodyRoot, &late.Commit)
	blk = nextBlock(t, hs, late)
	check("dated after the block", blk, viewFor(t, hc, blk), lowHash, ErrShareTimeTooNew)

	early := share(1, 3, mh.LaneC, 5)
	early.Header.Time = tip.Time - 1
	blk = nextBlock(t, hs, early)
	check("dated before its parent", blk, viewFor(t, hc, blk), lowHash, ErrTimeTooOld)

	// Out of order: swap two shares after the encoding put them in order.
	blk = nextBlock(t, hs, share(1, 1, mh.LaneC, 6), share(1, 1, mh.LaneC, 7))
	sh := blk.Settlement.Shares
	sh[0], sh[1] = sh[1], sh[0]
	check("out of order", blk, viewFor(t, hc, blk), lowHash, ErrShareOrder)
}

func TestSettlementStructure(t *testing.T) {
	_, hs, share := settleChain(t)
	base := nextBlock(t, hs, share(1, 1, mh.LaneC, 1), share(2, 2, mh.LaneG, 2)).Settlement
	if err := base.CheckStructure(); err != nil {
		t.Fatal(err)
	}
	copyOfS := func() Settlement {
		return Settlement{Keys: append([]SettleKey{}, base.Keys...), Jobs: append([]SettleJob{}, base.Jobs...),
			Shares: append([]SettleShare{}, base.Shares...)}
	}
	cases := map[string]func(s *Settlement){
		"depth 0":        func(s *Settlement) { s.Shares[0].Depth = 0 },
		"depth 9":        func(s *Settlement) { s.Shares[0].Depth = ShareExpiry + 1 },
		"a job too far":  func(s *Settlement) { s.Shares[0].Job = 5 },
		"an unused job":  func(s *Settlement) { s.Jobs = append(s.Jobs, SettleJob{Key: 0, Version: 9}) },
		"an unused key":  func(s *Settlement) { s.Keys = append(s.Keys, SettleKey{Payout: [32]byte{9}}) },
		"a job twice":    func(s *Settlement) { s.Jobs[1] = s.Jobs[0] },
		"a key twice":    func(s *Settlement) { s.Keys[1] = s.Keys[0] },
		"jobs reordered": func(s *Settlement) { s.Shares[0].Job, s.Shares[1].Job = 1, 0 },
		"heavy job":      func(s *Settlement) { s.Jobs[0].Weight = MaxShareWeight + 1 },
		"no lane":        func(s *Settlement) { s.Jobs[0].Lane = 7 },
	}
	for name, change := range cases {
		s := copyOfS()
		change(&s)
		if err := s.CheckStructure(); err == nil {
			t.Errorf("%s: taken", name)
		}
	}
	if _, err := NewSettlement([]*Share{share(1, 1, mh.LaneC, 1)}, uint64(hs[len(hs)-1].Height)+ShareExpiry+1); !errors.Is(err, ErrShareDepth) {
		t.Fatalf("a share 9 below the block: %v", err)
	}
}

type pickHash struct {
	PoWHasher
	pick map[int]bool
}

func (p pickHash) Checks(i int) bool { return p.pick[i] }

// A light node verifies the shares it samples: a sampled share without its PoW fails the block,
// the ones it did not pick are taken on trust.
func TestSettlementSampledPoW(t *testing.T) {
	hc, hs, share := settleChain(t)
	a, b2 := share(1, 1, mh.LaneC, 1), share(2, 2, mh.LaneG, 2)
	blk := nextBlock(t, hs, a, b2)
	bad := fixedHash{0xff}
	if _, err := blk.CheckSettlement(viewFor(t, hc, blk), pickHash{bad, map[int]bool{1: true}}, RegTest); !errors.Is(err, ErrSharePoW) {
		t.Fatalf("a sampled share without its PoW: %v", err)
	}
	if got, err := blk.CheckSettlement(viewFor(t, hc, blk), pickHash{bad, nil}, RegTest); err != nil || len(got) != 2 {
		t.Fatalf("nothing sampled: %d shares, %v", len(got), err)
	}
	if _, err := blk.CheckSettlement(viewFor(t, hc, blk), pickHash{lowHash, map[int]bool{0: true, 1: true}}, RegTest); err != nil {
		t.Fatalf("sampled shares with their PoW: %v", err)
	}
}
