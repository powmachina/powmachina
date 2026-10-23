package chain

import (
	"errors"
	"testing"

	"powmachina/internal/mh"
)

type countingHasher struct{ n *int }

func (c countingHasher) PoWHash(*Header) ([32]byte, error) { *c.n++; return [32]byte{0xaa}, nil }

func TestSharePool(t *testing.T) {
	_, hs, share := settleChain(t)
	tip := uint64(hs[len(hs)-1].Height)
	p := NewSharePool(3)
	a, b, c, d := share(1, 1, mh.LaneC, 1), share(3, 1, mh.LaneG, 2), share(7, 2, mh.LaneC, 3), share(8, 2, mh.LaneG, 4)
	for _, s := range []*Share{a, b, c} {
		if !p.Add(s, [32]byte{byte(s.Header.Nonce)}) {
			t.Fatalf("share %d was not taken", s.Header.Nonce)
		}
	}
	if p.Add(a, [32]byte{}) || p.Add(d, [32]byte{}) {
		t.Fatal("a duplicate or a share over the cap was taken")
	}

	// The cache answers what it has and asks for the rest.
	calls := 0
	h := p.PoWHash(countingHasher{&calls})
	if got, _ := h.PoWHash(&b.Header); got != ([32]byte{2}) || calls != 0 {
		t.Fatalf("a pooled share's hash: %x after %d calls", got[:1], calls)
	}
	if got, _ := h.PoWHash(&d.Header); got != ([32]byte{0xaa}) || calls != 1 {
		t.Fatal("a share the pool does not have was not computed")
	}

	// The oldest first; what is off the branch or settled stays out.
	all := p.Candidates(func([32]byte) bool { return true }, nil, 10)
	if len(all) != 3 || all[0] != c || all[2] != a {
		t.Fatal("the candidates are not oldest first")
	}
	if got := p.Candidates(func(prev [32]byte) bool { return prev != b.Header.PrevBlock },
		func(id [32]byte) bool { return id == a.ID() }, 10); len(got) != 1 || got[0] != c {
		t.Fatalf("%d candidates off the branch and settled filtered out, expected only the third", len(got))
	}
	if got := p.Candidates(func([32]byte) bool { return true }, nil, 2); len(got) != 2 || got[0] != c {
		t.Fatal("the cap did not keep the oldest")
	}

	// c builds 7 below the next block: still in. One block later it is 8 below the next: still in,
	// it is the deepest a block may include. One more and it leaves.
	if p.Expire(tip) != 0 || p.Expire(tip+1) != 0 || p.Expire(tip+2) != 1 || p.Has(c.ID()) {
		t.Fatal("the expiry is not the inclusion depth")
	}
	if _, err := NewSettlement([]*Share{c}, tip+3); !errors.Is(err, ErrShareDepth) {
		t.Fatal("the expired share could still be encoded")
	}
}
