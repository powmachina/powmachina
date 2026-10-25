package chain

// The credit step as it was before the state got its indexes and its stored tree: every block
// read the whole state, planned the payouts over all of it and hashed the whole tree. Kept as the
// reference the indexed one is checked against (TestCreditStepMatchesTheReference).

import (
	"bytes"
	"errors"
	"math/bits"
	"slices"
)

type refView interface {
	Pot() int64
	ForEachCredit(fn func([32]byte, Credit) error) error
}

func refReadCredits(v refView) (map[[32]byte]Credit, error) {
	state := make(map[[32]byte]Credit)
	if err := v.ForEachCredit(func(p [32]byte, c Credit) error {
		state[p] = c
		return nil
	}); err != nil {
		// Reading the state failed, the block did not: it says nothing about its validity.
		return nil, errors.Join(ErrSettlementUnchecked, ErrCreditState, err)
	}
	return state, nil
}

func refPlanPayouts(height uint64, state map[[32]byte]Credit) payoutPlan {
	type queued struct {
		payout   [32]byte
		from     uint64
		inactive bool
	}
	var plan payoutPlan
	var queue []queued
	for p, c := range state {
		if c.LastShare+InactiveBlocks <= height {
			if c.Amount >= CreditDust {
				queue = append(queue, queued{p, c.LastShare + InactiveBlocks, true})
			} else {
				plan.dust = append(plan.dust, p)
			}
			continue
		}
		if c.ReadyAt == 0 || (c.PaidAt != 0 && height <= c.PaidAt+PayoutInterval) {
			continue
		}
		from := c.ReadyAt
		if c.PaidAt != 0 && c.PaidAt+PayoutInterval+1 > from {
			from = c.PaidAt + PayoutInterval + 1
		}
		queue = append(queue, queued{p, from, false})
	}
	slices.SortFunc(queue, func(a, b queued) int {
		if a.from != b.from {
			if a.from < b.from {
				return -1
			}
			return 1
		}
		return bytes.Compare(a.payout[:], b.payout[:])
	})
	budget := CoinbaseStorageMassReserve
	if dev := DevFee(height); dev > 0 {
		budget -= StorageMassC / uint64(dev)
	}
	for _, q := range queue {
		amount := state[q.payout].Amount
		mass := StorageMassC / uint64(amount)
		if len(plan.payouts) == MaxPayoutsPerBlock || mass > budget {
			break
		}
		budget -= mass
		plan.payouts = append(plan.payouts, TxOut{Value: amount, ScriptVersion: ScriptVersionBasic,
			Script: append([]byte(nil), q.payout[:]...)})
		plan.paid = append(plan.paid, q.payout)
		plan.inactive = append(plan.inactive, q.inactive)
	}
	return plan
}

// CreditPayouts are the outputs the coinbase of the block at `height` must pay after the dev fee,
// in this order: the front of the payout queue of the state `v` it builds on (the Sharepool spec §5.2).
// The credits that became payable first go first, a tie broken by the payout program; the coinbase
// pays for as long as MaxPayoutsPerBlock and the storage mass reserve last.
func refCreditPayouts(height uint64, v refView) ([]TxOut, error) {
	state, err := refReadCredits(v)
	if err != nil {
		return nil, err
	}
	return refPlanPayouts(height, state).payouts, nil
}

// CreditStep is the one implementation of what a block at `height`, with its own share `commit`,
// its settlement `s` and `fees` in transaction fees, does to the credit state `v` of its parent:
//
//  1. the coinbase pays the front of the payout queue (CreditPayouts),
//  2. inactive keys below the dust limit return their credit to the pot,
//  3. the block's reward less the dev fee, and its fees, go into the pot,
//  4. every share unit the block settles is credited pot / PotPriceDivisor, taken from the pot.
func refCreditStep(height uint64, commit *ShareCommit, s *Settlement, fees int64, v refView) (*CreditStepResult, error) {
	state, err := refReadCredits(v)
	if err != nil {
		return nil, err
	}
	before := make(map[[32]byte]*Credit)
	pot := v.Pot()
	changed := make(map[[32]byte]bool)
	touch := func(p [32]byte) {
		if changed[p] {
			return
		}
		changed[p] = true
		if c, ok := state[p]; ok {
			before[p] = &c
		} else {
			before[p] = nil
		}
	}

	// 1 + 2.
	plan := refPlanPayouts(height, state)
	for _, p := range plan.dust {
		touch(p)
		pot += state[p].Amount
		delete(state, p)
	}
	for i, p := range plan.paid {
		touch(p)
		if plan.inactive[i] {
			delete(state, p)
			continue
		}
		c := state[p]
		c.Amount, c.PaidAt, c.ReadyAt = 0, height, 0
		state[p] = c
	}
	payouts := plan.payouts

	// 3: the reward and the fees.
	pot += Subsidy(height) - DevFee(height) + fees

	// 4: the price, and the credits.
	units, total := settledUnits(commit, s)
	price := pot / PotPriceDivisor
	hi, lo := bits.Mul64(uint64(price), total)
	byPrice := hi == 0 && lo <= uint64(pot)
	potBefore := pot
	for p, u := range units {
		var credit int64
		if byPrice {
			credit = price * int64(u.units)
		} else {
			// More units than the pot covers at the price: the pot is split in proportion.
			h, l := bits.Mul64(uint64(potBefore), u.units)
			q, _ := bits.Div64(h, l, total)
			credit = int64(q)
		}
		pot -= credit
		touch(p)
		c := state[p]
		c.Amount += credit
		c.LastShare = height
		c.Threshold = u.threshold
		if c.Amount >= PayoutNeed(c.Threshold) {
			if c.ReadyAt == 0 {
				c.ReadyAt = height
			}
		} else {
			c.ReadyAt = 0
		}
		state[p] = c
	}

	res := &CreditStepResult{Payouts: payouts, Changed: make(map[[32]byte]*Credit, len(changed)),
		Before: before, Pot: pot}
	for p := range changed {
		if c, ok := state[p]; ok {
			res.Changed[p] = &c
		} else {
			res.Changed[p] = nil
		}
	}
	res.Root = CreditsRoot(pot, state)
	return res, nil
}
