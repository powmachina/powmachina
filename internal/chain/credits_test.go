package chain

import (
	"errors"
	"testing"
)

// creditState is a CreditView over a map, for the tests.
type creditState struct {
	pot     int64
	credits map[[32]byte]Credit
}

func (s *creditState) Pot() int64            { return s.pot }
func (s *creditState) CreditsRoot() [32]byte { return CreditsRoot(s.pot, s.credits) }
func (s *creditState) ForEachCredit(fn func([32]byte, Credit) error) error {
	for p, c := range s.credits {
		if err := fn(p, c); err != nil {
			return err
		}
	}
	return nil
}

// apply moves the state by a step, the way a store does.
func (s *creditState) apply(r *CreditStepResult) {
	for p, c := range r.Changed {
		if c == nil {
			delete(s.credits, p)
		} else {
			s.credits[p] = *c
		}
	}
	s.pot = r.Pot
}

func payoutKey(b byte) [32]byte { return [32]byte{b, 0x5a} }

// A block pays each unit pot / 48 000, its own share's included; what it does not pay stays.
func TestCreditStepPaysTheFixedPrice(t *testing.T) {
	const h = 30_000 // past the slow start, inside the dev fee
	s := &creditState{pot: 7 * POWM, credits: map[[32]byte]Credit{}}
	commit := &ShareCommit{Payout: payoutKey(1), Weight: MaxShareWeight}
	sett := &Settlement{
		Keys:   []SettleKey{{Payout: payoutKey(2)}},
		Jobs:   []SettleJob{{Key: 0, Weight: 3, Threshold: 2}},
		Shares: []SettleShare{{Job: 0}, {Job: 0, Nonce: 1}},
	}
	const fees = 12_345
	r, err := CreditStep(h, commit, sett, fees, s)
	if err != nil {
		t.Fatal(err)
	}
	pot := 7*POWM + MinerSubsidy(h) + fees
	price := pot / PotPriceDivisor
	own, other := r.Changed[payoutKey(1)], r.Changed[payoutKey(2)]
	if own == nil || own.Amount != price*2048 || own.LastShare != h {
		t.Fatalf("the closer: %+v, expected %d", own, price*2048)
	}
	if other == nil || other.Amount != price*16 || other.Threshold != 2 {
		t.Fatalf("two shares of weight 3: %+v, expected %d", other, price*16)
	}
	if r.Pot != pot-price*(2048+16) {
		t.Fatalf("the pot after: %d, expected %d", r.Pot, pot-price*(2048+16))
	}
	if len(r.Payouts) != 0 {
		t.Fatalf("an empty state paid %d outputs", len(r.Payouts))
	}
	// The undo names both keys as new.
	if b, ok := r.Before[payoutKey(1)]; !ok || b != nil {
		t.Fatal("the undo does not name the closer's key as new")
	}
}

// More units than the price covers split the pot in proportion, and never more than it holds.
func TestCreditStepSplitsAnOverfullBlock(t *testing.T) {
	const h = 30_000
	s := &creditState{pot: POWM, credits: map[[32]byte]Credit{}}
	sett := &Settlement{Keys: []SettleKey{{Payout: payoutKey(2)}}, Jobs: []SettleJob{{Weight: MaxShareWeight}}}
	for i := range 40 { // 40 × 2048 units, more than 48 000
		sett.Shares = append(sett.Shares, SettleShare{Nonce: uint64(i)})
	}
	r, err := CreditStep(h, &ShareCommit{Payout: payoutKey(1)}, sett, 0, s)
	if err != nil {
		t.Fatal(err)
	}
	pot := POWM + MinerSubsidy(h)
	paid := r.Changed[payoutKey(1)].Amount + r.Changed[payoutKey(2)].Amount
	if r.Pot < 0 || paid+r.Pot != pot || r.Pot >= 2 {
		t.Fatalf("paid %d, left %d, the pot was %d", paid, r.Pot, pot)
	}
	if got, want := r.Changed[payoutKey(1)].Amount, pot/(40*2048+1); got != want {
		t.Fatalf("one unit of %d got %d, expected %d", 40*2048+1, got, want)
	}
}

// The queue: the credits that became payable first go first; a key paid within the day waits; a
// credit below its threshold waits; the coinbase stops at MaxPayoutsPerBlock.
func TestCreditPayoutsQueue(t *testing.T) {
	const h = 100_000
	s := &creditState{credits: map[[32]byte]Credit{
		payoutKey(1): {Amount: POWM, LastShare: h - 1, ReadyAt: h - 50},
		payoutKey(2): {Amount: POWM, LastShare: h - 1, ReadyAt: h - 90},
		payoutKey(3): {Amount: POWM, LastShare: h - 1, ReadyAt: h - 90, PaidAt: h - PayoutInterval}, // paid a day ago, to the block
		payoutKey(4): {Amount: POWM, LastShare: h - 1, ReadyAt: h - 95, PaidAt: h - PayoutInterval - 1},
		payoutKey(5): {Amount: MinPayout - 1, LastShare: h - 1},
	}}
	got, err := CreditPayouts(h, s)
	if err != nil {
		t.Fatal(err)
	}
	// Key 4 became payable at its PaidAt + interval + 1 = h, after 2 (h-90) and 1 (h-50).
	want := [][32]byte{payoutKey(2), payoutKey(1), payoutKey(4)}
	if len(got) != len(want) {
		t.Fatalf("%d payouts, expected %d", len(got), len(want))
	}
	for i, p := range want {
		if string(got[i].Script) != string(p[:]) || got[i].Value != POWM {
			t.Fatalf("payout %d goes to %x, expected %x", i, got[i].Script[:2], p[:2])
		}
	}

	for i := range MaxPayoutsPerBlock + 10 {
		var p [32]byte
		p[0], p[1], p[2] = 0xEE, byte(i), byte(i>>8)
		s.credits[p] = Credit{Amount: MinPayout, LastShare: h - 1, ReadyAt: 1}
	}
	if got, _ := CreditPayouts(h, s); len(got) != MaxPayoutsPerBlock {
		t.Fatalf("%d payouts in one coinbase, the cap is %d", len(got), MaxPayoutsPerBlock)
	}
}

// A payout empties the credit and starts the key's day; a credit that reaches its threshold is
// ready from that block on, one above 2^MaxPayoutThreshold never.
func TestCreditStepPaysAndRearms(t *testing.T) {
	const h = 100_000
	s := &creditState{pot: 0, credits: map[[32]byte]Credit{
		payoutKey(1): {Amount: 2 * POWM, LastShare: h - 5, ReadyAt: h - 5},
	}}
	r, err := CreditStep(h, &ShareCommit{Payout: payoutKey(2), Threshold: 255}, nil, 0, s)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Payouts) != 1 || r.Payouts[0].Value != 2*POWM {
		t.Fatalf("payouts %+v", r.Payouts)
	}
	if c := r.Changed[payoutKey(1)]; c == nil || c.Amount != 0 || c.PaidAt != h || c.ReadyAt != 0 {
		t.Fatalf("the paid key: %+v", c)
	}
	if c := r.Changed[payoutKey(2)]; c == nil || c.ReadyAt != 0 {
		t.Fatalf("a key asking for 2^255 × 0.05 POWM became ready: %+v", c)
	}
	s.apply(r)
	if got, _ := CreditPayouts(h+1, s); len(got) != 0 {
		t.Fatal("a key was paid twice within its day")
	}
}

// A key that stopped mining 90 days ago: above the dust limit it is paid whatever its threshold,
// below it the credit returns to the pot; either way its entry goes.
func TestCreditStepInactiveKeys(t *testing.T) {
	const h = InactiveBlocks + 1000
	s := &creditState{pot: 0, credits: map[[32]byte]Credit{
		payoutKey(1): {Amount: CreditDust, Threshold: 255, LastShare: h - InactiveBlocks},
		payoutKey(2): {Amount: CreditDust - 1, LastShare: h - InactiveBlocks},
		payoutKey(3): {Amount: CreditDust - 1, LastShare: h - InactiveBlocks + 1},
	}}
	r, err := CreditStep(h, &ShareCommit{Payout: payoutKey(9)}, nil, 0, s)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Payouts) != 1 || r.Payouts[0].Value != CreditDust {
		t.Fatalf("payouts %+v", r.Payouts)
	}
	for _, p := range [][32]byte{payoutKey(1), payoutKey(2)} {
		if c, ok := r.Changed[p]; !ok || c != nil {
			t.Fatalf("the inactive key %x stayed: %+v", p[:1], c)
		}
	}
	if _, ok := r.Changed[payoutKey(3)]; ok {
		t.Fatal("a key one block short of inactive was touched")
	}
	pot := CreditDust - 1 + MinerSubsidy(h)
	if paid := r.Changed[payoutKey(9)].Amount; r.Pot+paid != pot {
		t.Fatalf("the dust did not return to the pot: %d + %d, expected %d", r.Pot, paid, pot)
	}
}

// The root: one value for one state whatever the order it was built in, a different one for any
// change; a lone entry is its leaf, two entries split on their first differing bit.
func TestCreditsRoot(t *testing.T) {
	a, b, c := [32]byte{0x00, 1}, [32]byte{0x80, 2}, [32]byte{0x40, 3}
	ca, cb, cc := Credit{Amount: 1}, Credit{Amount: 2}, Credit{Amount: 3}
	empty := CreditsRoot(0, nil)
	if empty != CreditsRoot(0, map[[32]byte]Credit{}) || empty == CreditsRoot(1, nil) {
		t.Fatal("the empty root depends on more than the pot")
	}
	tree := creditsRootFromTree
	if got := CreditsRoot(0, map[[32]byte]Credit{a: ca}); got != tree(0, CreditLeaf(a, ca)) {
		t.Fatal("a lone entry is not its own leaf")
	}
	// a = 0000…, c = 0100…, b = 1000…: the root splits a+c from b, then a from c.
	want := CreditNode(CreditNode(CreditLeaf(a, ca), CreditLeaf(c, cc)), CreditLeaf(b, cb))
	got := CreditsRoot(5, map[[32]byte]Credit{b: cb, c: cc, a: ca})
	if got != tree(5, want) {
		t.Fatal("the tree is not split on the bits of the key")
	}
	// Keys that share their first two bits hang under nodes with an empty sibling.
	d := [32]byte{0x20, 4}
	cd := Credit{Amount: 4}
	want = CreditNode(CreditNode(CreditNode(CreditLeaf(a, ca), CreditLeaf(d, cd)), [32]byte{}), [32]byte{})
	if CreditsRoot(5, map[[32]byte]Credit{a: ca, d: cd}) != tree(5, want) {
		t.Fatal("an empty sibling is not all zeros")
	}
	if CreditsRoot(5, map[[32]byte]Credit{a: ca, d: {Amount: 4}}) ==
		CreditsRoot(5, map[[32]byte]Credit{a: ca, d: {Amount: 5}}) {
		t.Fatal("a changed amount left the root as it was")
	}
	if !errors.Is(errors.Join(ErrSettlementUnchecked, ErrCreditState), ErrSettlementUnchecked) {
		t.Fatal("a state that could not be read must not look like a bad block")
	}
}

// The step a UTXOSet applies is the step CreditStep computes, its root is what the next coinbase
// must carry, and the undo puts every entry back.
func TestUTXOSetCarriesTheCredits(t *testing.T) {
	p := RegTest
	u := NewUTXOSet()
	var h uint64 = 25_000
	// coinbase pays what the queue of u owes at height h.
	coinbase := func(h uint64) *Tx {
		cb := coinbaseTx(h, 0, p)
		payouts, err := CreditPayouts(h, u)
		if err != nil {
			t.Fatal(err)
		}
		cb.Outputs = append(cb.Outputs, payouts...)
		return cb
	}
	paid := 0
	for i := range 3 {
		b := blockWith(h, []*Tx{coinbase(h)})
		paid += len(b.Txs[0].Outputs) - 2
		b.Share = ShareCommit{Payout: payoutKey(byte(i % 2)), Weight: MaxShareWeight}
		SealCoinbase(b.Txs, u.CreditsRoot())
		b.Seal()
		if err := b.ValidateBlock(u, p); err != nil {
			t.Fatalf("block %d: %v", i, err)
		}
		rootBefore, potBefore := u.CreditsRoot(), u.Pot()
		undo, err := u.Apply(b)
		if err != nil {
			t.Fatal(err)
		}
		state := map[[32]byte]Credit{}
		_ = u.ForEachCredit(func(k [32]byte, c Credit) error { state[k] = c; return nil })
		if u.CreditsRoot() != CreditsRoot(u.Pot(), state) {
			t.Fatal("the stored root is not the root of the stored state")
		}
		// A coinbase committing to the state before the block is refused on top of it.
		stale := blockWith(h+1, []*Tx{coinbase(h + 1)})
		SealCoinbase(stale.Txs, rootBefore)
		stale.Seal()
		if err := stale.ValidateBlock(u, p); !errors.Is(err, ErrCreditRoot) {
			t.Fatalf("a stale credit root: %v, expected ErrCreditRoot", err)
		}
		if i == 2 {
			u.Undo(undo)
			if u.CreditsRoot() != rootBefore || u.Pot() != potBefore {
				t.Fatal("the undo did not restore the credit state")
			}
		}
		h++
	}
	// Each key got ~0.4 POWM from its first block, and the next block paid it.
	if paid != 2 {
		t.Fatalf("%d payouts in three blocks, expected 2", paid)
	}
}
