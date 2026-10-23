package chain

import (
	"testing"
)

// siblings returns two headers on the tip's parent, the one with the lower ID first.
func siblings(t *testing.T, hc *HeaderChain, parent *Header) (low, high *Header) {
	t.Helper()
	a, b := sideHeader(t, hc, parent), sideHeader(t, hc, parent)
	b.Nonce++
	if lessID(a.ID(), b.ID()) {
		return a, b
	}
	return b, a
}

// Of two siblings the lower ID is the tip, whichever came first — every node ends up on the same
// one as soon as it has seen both.
func TestSiblingWithTheLowerIDWins(t *testing.T) {
	for _, lowFirst := range []bool{true, false} {
		hc, hs := mainChain(t, RegTest, 20)
		low, high := siblings(t, hc, hs[19])
		first, second := high, low
		if lowFirst {
			first, second = low, high
		}
		if _, err := hc.AddHeader(first, acceptPoW{}); err != nil {
			t.Fatal(err)
		}
		if _, err := hc.AddHeader(second, acceptPoW{}); err != nil {
			t.Fatal(err)
		}
		// hs[20] is a third sibling: the tip is whichever of the three has the lowest ID.
		want := low.ID()
		if lessID(hs[20].ID(), want) {
			want = hs[20].ID()
		}
		if _, _, tip := hc.Tip(); tip != want {
			t.Fatalf("lowFirst=%v: the tip is %x, want the lowest ID %x", lowFirst, tip[:4], want[:4])
		}
	}
}

// Votes beat the lower ID only by more than the margin, and a vote that arrives later moves the
// tip with ReconsiderSibling.
func TestSiblingVotesNeedMoreThanTheMargin(t *testing.T) {
	hc, hs := mainChain(t, RegTest, 20)
	votes := map[[32]byte]uint64{}
	hc.SetVotes(func(id [32]byte) uint64 { return votes[id] })

	low, high := siblings(t, hc, hs[19])
	if _, err := hc.AddHeader(low, acceptPoW{}); err != nil {
		t.Fatal(err)
	}
	if _, err := hc.AddHeader(high, acceptPoW{}); err != nil {
		t.Fatal(err)
	}
	// The loaded hs[20] is a third sibling; the test starts from `low` as the tip.
	hc.mu.Lock()
	hc.tip = hc.byID[low.ID()]
	hc.mu.Unlock()

	votes[high.ID()] = SiblingVoteMargin
	if hc.ReconsiderSibling(high.ID()) {
		t.Fatal("a lead of exactly the margin beat the lower ID")
	}
	votes[high.ID()] = SiblingVoteMargin + 1
	if !hc.ReconsiderSibling(high.ID()) {
		t.Fatal("a lead of more than the margin did not move the tip")
	}
	if _, _, tip := hc.Tip(); tip != high.ID() {
		t.Fatalf("the tip is %x, want the voted sibling", tip[:4])
	}

	// The lower one catches up to within the margin: the tie-break gives it back the tip.
	votes[low.ID()] = 2
	if !hc.ReconsiderSibling(low.ID()) {
		t.Fatal("within the margin the lower ID did not win back")
	}
	// A block that is not a sibling of the tip is never switched to this way.
	if hc.ReconsiderSibling(hs[19].ID()) {
		t.Fatal("the tip's parent was taken for a sibling")
	}
}

// A longer branch is chainwork's business: votes for a sibling do not hold back a child of the other.
func TestSiblingRuleDoesNotOutweighALongerBranch(t *testing.T) {
	hc, hs := mainChain(t, RegTest, 20)
	votes := map[[32]byte]uint64{}
	hc.SetVotes(func(id [32]byte) uint64 { return votes[id] })
	low, high := siblings(t, hc, hs[19])
	for _, h := range []*Header{low, high} {
		if _, err := hc.AddHeader(h, acceptPoW{}); err != nil {
			t.Fatal(err)
		}
	}
	votes[low.ID()] = 1 << 20
	child := sideHeader(t, hc, high)
	if moved, err := hc.AddHeader(child, acceptPoW{}); err != nil || !moved {
		t.Fatalf("a child of the other sibling: moved=%v err=%v", moved, err)
	}
	if hc.ReconsiderSibling(low.ID()) {
		t.Fatal("votes for a block one shorter than the tip moved it")
	}
}

// The pool counts a share's units for the block it builds on, and forgets them when it expires.
func TestSharePoolCountsVotes(t *testing.T) {
	p := NewSharePool(10)
	var on [32]byte
	on[0] = 7
	s1 := &Share{Header: Header{PrevBlock: on, Height: 5}, Commit: ShareCommit{Weight: 3}}
	s2 := &Share{Header: Header{PrevBlock: on, Height: 5, Nonce: 1}, Commit: ShareCommit{Weight: 0}}
	p.Add(s1, [32]byte{})
	p.Add(s2, [32]byte{})
	p.Add(s2, [32]byte{}) // a duplicate is not a second vote
	if v := p.Votes(on); v != 9 {
		t.Fatalf("votes %d, want 8 + 1", v)
	}
	p.Expire(5 + ShareExpiry)
	if v := p.Votes(on); v != 0 {
		t.Fatalf("votes %d after the shares expired", v)
	}
}
