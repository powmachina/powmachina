package chain

import (
	"math/rand/v2"
	"reflect"
	"testing"

	"powmachina/internal/mh"
)

// The indexed credit step has to do exactly what the step that read the whole state did: the
// same payouts in the same order, the same entries, the same pot and the same root. A difference
// here is a fork. The states are random, with keys that share long prefixes (deep trees), credits
// around the dust limit and the payout, inactive keys and full coinbases.
func TestCreditStepMatchesTheReference(t *testing.T) {
	for seed := range uint64(40) {
		rng := rand.New(rand.NewPCG(seed, 0xC0FFEE))
		h := InactiveBlocks + 50_000 + uint64(rng.IntN(1000))
		ref := &creditState{pot: int64(rng.IntN(100)) * POWM, credits: map[[32]byte]Credit{}}
		var known [][32]byte
		for range rng.IntN(300) {
			p := randomPayout(rng, known)
			known = append(known, p)
			ref.credits[p] = randomCredit(rng, h, seed%4 == 0)
		}
		mem := ref.copyToMem()
		type step struct {
			undo  *BlockUndo
			state map[[32]byte]Credit
			nodes map[CreditNodeKey][32]byte
			pot   int64
		}
		var steps []step
		for range 30 {
			commit, s := randomSettlement(rng, &known)
			fees := int64(rng.IntN(1000))
			want, err := refCreditStep(h, commit, s, fees, ref)
			if err != nil {
				t.Fatal(err)
			}
			wantRoot := CreditsRoot(want.Pot, applied(ref.credits, want.Changed))
			got, err := CreditStep(h, commit, s, fees, mem)
			if err != nil {
				t.Fatalf("seed %d height %d: %v", seed, h, err)
			}
			if !reflect.DeepEqual(got.Payouts, want.Payouts) {
				t.Fatalf("seed %d height %d: the payouts differ: %d vs %d", seed, h, len(got.Payouts), len(want.Payouts))
			}
			if !sameCredits(got.Changed, want.Changed) || !sameCredits(got.Before, want.Before) {
				t.Fatalf("seed %d height %d: the changed entries differ", seed, h)
			}
			if got.Pot != want.Pot || got.Root != wantRoot {
				t.Fatalf("seed %d height %d: pot %d root %x, the reference %d %x", seed, h, got.Pot, got.Root[:6], want.Pot, wantRoot[:6])
			}
			steps = append(steps, step{
				undo:  &BlockUndo{Height: h, Credits: got.Before, Pot: mem.pot, CreditsRoot: mem.root},
				state: copyCredits(mem.credits), nodes: copyNodes(mem.nodes), pot: mem.pot,
			})
			ref.apply(want)
			for p, c := range got.Changed {
				mem.PutCredit(p, c)
			}
			for k, n := range got.Nodes {
				mem.PutCreditNode(k, n)
			}
			mem.PutPot(got.Pot, got.Root)
			// The stored tree is the tree of the state, node for node.
			if !reflect.DeepEqual(mem.nodes, ref.view().nodes) {
				t.Fatalf("seed %d height %d: the stored nodes are not the state's tree", seed, h)
			}
			h += 1 + uint64(rng.IntN(3000))
		}
		// Unwinding the blocks gives back every state on the way, its nodes included.
		for i := len(steps) - 1; i >= 0; i-- {
			if err := RevertCredits(mem, steps[i].undo); err != nil {
				t.Fatalf("seed %d: revert %d: %v", seed, i, err)
			}
			if !reflect.DeepEqual(mem.credits, steps[i].state) || !reflect.DeepEqual(mem.nodes, steps[i].nodes) || mem.pot != steps[i].pot {
				t.Fatalf("seed %d: revert %d did not give back the state", seed, i)
			}
		}
	}
}

// A queue longer than one read: the planner reads more until it has what the coinbase pays.
func TestCreditPayoutsBeyondOneRead(t *testing.T) {
	const h = InactiveBlocks + 100_000
	rng := rand.New(rand.NewPCG(7, 7))
	ref := &creditState{credits: map[[32]byte]Credit{}}
	for i := range 900 {
		var p [32]byte
		fillRandom(rng, p[:])
		c := Credit{Amount: 10 * POWM, LastShare: h - 1, ReadyAt: uint64(1 + rng.IntN(5000))}
		if i%3 == 0 {
			c.LastShare = h - InactiveBlocks - uint64(rng.IntN(5000)) // inactive, paid by its expiry
		}
		ref.credits[p] = c
	}
	want, _ := refCreditPayouts(h, ref)
	got, err := CreditPayouts(h, ref.copyToMem())
	if err != nil {
		t.Fatal(err)
	}
	if len(want) != MaxPayoutsPerBlock || !reflect.DeepEqual(got, want) {
		t.Fatalf("%d payouts, the reference %d", len(got), len(want))
	}
}

func (s *creditState) copyToMem() *memCredits {
	v := s.view()
	return &memCredits{credits: copyCredits(s.credits), nodes: v.nodes, pot: s.pot, root: v.root}
}

func randomPayout(rng *rand.Rand, known [][32]byte) [32]byte {
	var p [32]byte
	if len(known) > 0 && rng.IntN(3) == 0 {
		// Next to a known one: the same first bytes, so the tree gets deep.
		p = known[rng.IntN(len(known))]
		bit := rng.IntN(256)
		p[bit/8] ^= 1 << (7 - bit%8)
		for d := bit + 1; d < 256; d++ {
			if rng.IntN(2) == 0 {
				p[d/8] ^= 1 << (7 - d%8)
			}
		}
		return p
	}
	fillRandom(rng, p[:])
	return p
}

func randomCredit(rng *rand.Rand, h uint64, rich bool) Credit {
	amounts := []int64{0, CreditDust - 1, CreditDust, MinPayout - 1, MinPayout, 2 * MinPayout, 3 * POWM}
	c := Credit{Amount: amounts[rng.IntN(len(amounts))], Threshold: uint8(rng.IntN(3))}
	if rich {
		c.Amount = 5 * POWM
	}
	if rng.IntN(20) == 0 {
		c.Threshold = MaxPayoutThreshold + 1
	}
	c.LastShare = h - 1 - uint64(rng.IntN(int(InactiveBlocks)+100))
	if rng.IntN(2) == 0 {
		c.PaidAt = c.LastShare - uint64(rng.IntN(4000))
	}
	if c.Amount >= PayoutNeed(c.Threshold) {
		c.ReadyAt = c.LastShare - uint64(rng.IntN(100))
	}
	return c
}

func randomSettlement(rng *rand.Rand, known *[][32]byte) (*ShareCommit, *Settlement) {
	pick := func() [32]byte {
		if len(*known) > 0 && rng.IntN(2) == 0 {
			return (*known)[rng.IntN(len(*known))]
		}
		p := randomPayout(rng, *known)
		*known = append(*known, p)
		return p
	}
	commit := &ShareCommit{Payout: pick(), Weight: uint8(rng.IntN(MaxShareWeight + 1)), Threshold: uint8(rng.IntN(3))}
	s := &Settlement{}
	for range rng.IntN(25) {
		s.Keys = append(s.Keys, SettleKey{Payout: pick()})
		s.Jobs = append(s.Jobs, SettleJob{Key: uint16(len(s.Keys) - 1), Lane: mh.Lane(rng.IntN(2)),
			Weight: uint8(rng.IntN(4)), Threshold: uint8(rng.IntN(3))})
		s.Shares = append(s.Shares, SettleShare{Job: uint16(len(s.Jobs) - 1)})
	}
	return commit, s
}

func applied(state map[[32]byte]Credit, changed map[[32]byte]*Credit) map[[32]byte]Credit {
	out := copyCredits(state)
	for p, c := range changed {
		if c == nil {
			delete(out, p)
		} else {
			out[p] = *c
		}
	}
	return out
}

func sameCredits(a, b map[[32]byte]*Credit) bool {
	if len(a) != len(b) {
		return false
	}
	for p, c := range a {
		d, ok := b[p]
		if !ok || (c == nil) != (d == nil) || (c != nil && *c != *d) {
			return false
		}
	}
	return true
}

func copyCredits(m map[[32]byte]Credit) map[[32]byte]Credit {
	out := make(map[[32]byte]Credit, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func copyNodes(m map[CreditNodeKey][32]byte) map[CreditNodeKey][32]byte {
	out := make(map[CreditNodeKey][32]byte, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func fillRandom(rng *rand.Rand, b []byte) {
	for i := range b {
		b[i] = byte(rng.Uint32())
	}
}
