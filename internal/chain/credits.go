package chain

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math"
	"math/bits"
	"slices"

	"powmachina/internal/mh"
)

// Credits — what the Sharepool owes each miner (the Sharepool spec §3.2, §5).
//
// A block does not pay its miner. Its reward goes into the **pot**, and the block pays every share
// unit it settles — its own share and the ones in its settlement body — a fixed price out of the
// pot. What a share earns is a **credit** of its payout program in the chain's state; the coinbase
// pays credits out as ordinary outputs once they are large enough and their key's day has come.
//
// The transition is one function, CreditStep, used by validation, by the template and by both
// state stores — two versions of this arithmetic would be a fork waiting to happen (CLAUDE.md,
// rule 1). Integer arithmetic only.

const (
	// PotPriceDivisor: a share unit is worth pot / (16 × 3000) — the pot holds about sixteen blocks
	// of reward at rest (SHAREPOOL.md §3.2).
	PotPriceDivisor = 16 * SharesPerLaneBlock
	// MinPayout is the smallest credit the coinbase pays out, 0.05 POWM (§5.2).
	MinPayout int64 = 5 * POWM / 100
	// MaxPayoutThreshold: a threshold above this asks for more than there will ever be, and such a
	// credit is paid only when its key stops mining (InactiveBlocks).
	MaxPayoutThreshold = 40
	// PayoutInterval: a key is paid at most once in this many blocks (24 h).
	PayoutInterval uint64 = 2880
	// MaxPayoutsPerBlock caps the payout outputs of one coinbase.
	MaxPayoutsPerBlock = 400
	// InactiveBlocks: a key without a settled share for 90 days is paid whatever it has above the
	// dust limit, whatever its threshold; below the dust limit its credit returns to the pot.
	InactiveBlocks uint64 = 90 * 2880
	// CreditDust is the dust limit of an inactive key's payout — the mempool's, so the coinbase
	// creates no output a transaction could not.
	CreditDust = DustLimit
)

const (
	creditLeafTag  = "PowMachina/credit/leaf/v1"
	creditNodeTag  = "PowMachina/credit/node/v1"
	creditsRootTag = "PowMachina/credits/v1"
)

var ErrCreditState = errors.New("chain: the credit state could not be read")

// Credit is one key's entry in the state.
type Credit struct {
	Amount    int64
	Threshold uint8
	// LastShare is the height of the last block that settled a share of the key.
	LastShare uint64
	// PaidAt is the height of the key's last payout, 0 = never paid.
	PaidAt uint64
	// ReadyAt is the height at which the credit last reached what its threshold asks for, 0 while
	// it is below. The payout queue is in its order.
	ReadyAt uint64
}

// CreditBytes is the size of a serialized Credit.
const CreditBytes = 8 + 1 + 8 + 8 + 8

// Serialize is the credit's fixed form, the one its leaf in the root commits to.
func (c Credit) Serialize() [CreditBytes]byte {
	var b [CreditBytes]byte
	binary.LittleEndian.PutUint64(b[0:8], uint64(c.Amount))
	b[8] = c.Threshold
	binary.LittleEndian.PutUint64(b[9:17], c.LastShare)
	binary.LittleEndian.PutUint64(b[17:25], c.PaidAt)
	binary.LittleEndian.PutUint64(b[25:33], c.ReadyAt)
	return b
}

// DeserializeCredit reads a credit stored by Serialize.
func DeserializeCredit(b []byte) (Credit, error) {
	if len(b) != CreditBytes {
		return Credit{}, ErrCreditState
	}
	return Credit{
		Amount:    int64(binary.LittleEndian.Uint64(b[0:8])),
		Threshold: b[8],
		LastShare: binary.LittleEndian.Uint64(b[9:17]),
		PaidAt:    binary.LittleEndian.Uint64(b[17:25]),
		ReadyAt:   binary.LittleEndian.Uint64(b[25:33]),
	}, nil
}

// PayoutNeed is what a credit must reach before the coinbase pays it: 0.05 POWM × 2^threshold.
func PayoutNeed(threshold uint8) int64 {
	if threshold > MaxPayoutThreshold {
		return math.MaxInt64
	}
	return MinPayout << threshold
}

// CreditView reads the credit state a block builds on — the state its parent left.
//
// A block touches a few entries of a state that may hold millions, so nothing here walks the
// whole state: a block reads the entries it changes, the front of the payout queue and the stored
// nodes of the tree on the paths it changes (credittree.go, creditqueue.go).
type CreditView interface {
	Pot() int64
	// CreditsRoot is CreditsRoot of this state. A store keeps it next to the state, so a block
	// is checked against it without walking every entry.
	CreditsRoot() [32]byte
	// Credit reads one entry.
	Credit(payout [32]byte) (Credit, bool, error)
	// CreditsUnder returns, in the order of the programs, at most `max` entries whose program
	// starts with the first `depth` bits of `prefix`.
	CreditsUnder(prefix [32]byte, depth, max int) ([]CreditEntry, error)
	// CreditNode is the stored hash of the subtree at the first `depth` bits of `prefix`; there is
	// one exactly when the subtree holds two entries or more.
	CreditNode(prefix [32]byte, depth int) ([32]byte, bool, error)
	// PayoutQueue returns, in order, at most `max` entries of the payout queue (CreditQueueAt) that
	// are due at `height`, and whether those are all of them.
	PayoutQueue(height uint64, max int) ([]QueueEntry, bool, error)
	// ExpiredQueue does the same for the keys inactive at `height` (CreditExpiry) whose credit is
	// not dust; ExpiredDust returns every inactive key whose credit is.
	ExpiredQueue(height uint64, max int) ([]QueueEntry, bool, error)
	ExpiredDust(height uint64) ([]QueueEntry, error)
}

// CreditEntry is one entry of the state.
type CreditEntry struct {
	Payout [32]byte
	Credit Credit
}

// QueueEntry is a key in one of the queues, at the height it is due from.
type QueueEntry struct {
	At     uint64
	Payout [32]byte
}

// CreditQueueAt is the height from which a credit that reached its payout may be paid: when it got
// there, but not within PayoutInterval of the key's last payout. ok is false for a credit that has
// not reached it. A store keeps its payout queue in this order, the program breaking a tie.
func CreditQueueAt(c Credit) (uint64, bool) {
	if c.ReadyAt == 0 {
		return 0, false
	}
	from := c.ReadyAt
	if c.PaidAt != 0 && c.PaidAt+PayoutInterval+1 > from {
		from = c.PaidAt + PayoutInterval + 1
	}
	return from, true
}

// CreditExpiry is the height from which a key counts as inactive: InactiveBlocks after its last
// settled share. Then it is paid what it has above the dust limit, whatever its threshold, and
// below it its credit returns to the pot (CreditIsDust).
func CreditExpiry(c Credit) uint64 { return c.LastShare + InactiveBlocks }

// CreditIsDust says whether an inactive key's credit is too small to pay out.
func CreditIsDust(c Credit) bool { return c.Amount < CreditDust }

// CreditStepResult is what a block does to the credit state.
type CreditStepResult struct {
	// Payouts are the outputs the coinbase paid after the dev fee, in this order.
	Payouts []TxOut
	// Changed are the entries the block touched: the new value, or nil for one it deleted.
	Changed map[[32]byte]*Credit
	// Before are the same entries as they were, nil for one the block created — the undo.
	Before map[[32]byte]*Credit
	// Pot is the pot after the block.
	Pot int64
	// Root is the commitment to the state after the block, the one its child's coinbase carries.
	Root [32]byte
	// Nodes are the stored nodes of the tree that change with the block (nil = gone).
	Nodes map[CreditNodeKey]*[32]byte
}

// creditUnits is what a block settles per payout program: the units, and the threshold of the
// key's last share in the block's order (the settlement's shares, then the block's own).
type creditUnits struct {
	units     uint64
	threshold uint8
}

// settledUnits sums the share units a block pays per payout program: every share of its
// settlement body and the block itself, which is the share that also met the block's target.
// The body must have passed CheckStructure.
func settledUnits(commit *ShareCommit, s *Settlement) (map[[32]byte]creditUnits, uint64) {
	out := make(map[[32]byte]creditUnits)
	var total uint64
	add := func(payout [32]byte, weight, threshold uint8) {
		u := uint64(1) << weight
		e := out[payout]
		e.units += u
		e.threshold = threshold
		out[payout] = e
		total += u
	}
	if s != nil {
		for _, sh := range s.Shares {
			job := s.Jobs[sh.Job]
			add(s.Keys[job.Key].Payout, job.Weight, job.Threshold)
		}
	}
	add(commit.Payout, commit.Weight, commit.Threshold)
	return out, total
}

// creditReader reads entries of a view once and remembers them, with what a step has changed on
// top: nil is an entry the step deleted.
type creditReader struct {
	v    CreditView
	read map[[32]byte]*Credit // as the view has them, nil = none
	now  map[[32]byte]*Credit // after the step's changes so far
}

func newCreditReader(v CreditView) *creditReader {
	return &creditReader{v: v, read: make(map[[32]byte]*Credit), now: make(map[[32]byte]*Credit)}
}

func (r *creditReader) original(p [32]byte) (*Credit, error) {
	if c, ok := r.read[p]; ok {
		return c, nil
	}
	c, ok, err := r.v.Credit(p)
	if err != nil {
		// Reading the state failed, the block did not: it says nothing about its validity.
		return nil, errors.Join(ErrSettlementUnchecked, ErrCreditState, err)
	}
	var out *Credit
	if ok {
		out = &c
	}
	r.read[p] = out
	return out, nil
}

// get is the entry as the step sees it now.
func (r *creditReader) get(p [32]byte) (*Credit, error) {
	if c, ok := r.now[p]; ok {
		return c, nil
	}
	return r.original(p)
}

func stateErr(err error) error {
	if err == nil {
		return nil
	}
	return errors.Join(ErrSettlementUnchecked, ErrCreditState, err)
}

// payoutPlan is what the coinbase of the block at `height` pays out of the state: the front of the
// queue, and which of the paid keys are inactive (their entries go). `dust` are the inactive keys
// whose credit is below the dust limit — it returns to the pot.
type payoutPlan struct {
	payouts  []TxOut
	paid     [][32]byte
	inactive []bool
	dust     [][32]byte
}

// planPayouts takes the keys in the order they became payable — a key that reached its payout
// at CreditQueueAt, an inactive one at CreditExpiry — and pays from the front for as long as
// MaxPayoutsPerBlock and the coinbase's storage mass reserve last. It reads only the front of
// the two queues; when it needs more of them than it read, it reads again, more.
func planPayouts(height uint64, r *creditReader) (payoutPlan, error) {
	dust, err := r.v.ExpiredDust(height)
	if err != nil {
		return payoutPlan{}, stateErr(err)
	}
	for n := 64; ; n *= 8 {
		ready, readyAll, err := r.v.PayoutQueue(height, n)
		if err != nil {
			return payoutPlan{}, stateErr(err)
		}
		expired, expiredAll, err := r.v.ExpiredQueue(height, n)
		if err != nil {
			return payoutPlan{}, stateErr(err)
		}
		plan, short, err := mergePayouts(height, r, ready, readyAll, expired, expiredAll)
		if err != nil {
			return payoutPlan{}, err
		}
		if !short {
			for _, d := range dust {
				plan.dust = append(plan.dust, d.Payout)
			}
			return plan, nil
		}
	}
}

func queueLess(a, b QueueEntry) bool {
	if a.At != b.At {
		return a.At < b.At
	}
	return bytes.Compare(a.Payout[:], b.Payout[:]) < 0
}

// mergePayouts walks the two queues as one; short says it ran out of an incomplete one.
func mergePayouts(height uint64, r *creditReader, ready []QueueEntry, readyAll bool,
	expired []QueueEntry, expiredAll bool) (payoutPlan, bool, error) {
	var plan payoutPlan
	budget := CoinbaseStorageMassReserve
	if dev := DevFee(height); dev > 0 {
		// Saturate rather than wrap: with today's parameters the dev fee in the fee era keeps this
		// subtrahend tiny, but a future parameter change that let it exceed the reserve would, on a
		// uint64, wrap to a near-MaxUint64 budget and turn mass-limited payouts into count-limited ones.
		if sub := StorageMassC / uint64(dev); sub < budget {
			budget -= sub
		} else {
			budget = 0
		}
	}
	i, j := 0, 0
	for len(plan.payouts) < MaxPayoutsPerBlock {
		// The next key of the payout queue that is not inactive: an inactive key is paid from the
		// other queue, by its expiry.
		var next *QueueEntry
		var nextC *Credit
		for next == nil && i < len(ready) {
			c, err := r.original(ready[i].Payout)
			if err != nil {
				return plan, false, err
			}
			if c == nil {
				return plan, false, stateErr(errors.New("the payout queue names a key without a credit"))
			}
			if CreditExpiry(*c) <= height {
				i++
				continue
			}
			next, nextC = &ready[i], c
		}
		if next == nil && !readyAll {
			return plan, true, nil
		}
		if j >= len(expired) && !expiredAll {
			return plan, true, nil
		}
		inactive := false
		if j < len(expired) && (next == nil || queueLess(expired[j], *next)) {
			c, err := r.original(expired[j].Payout)
			if err != nil {
				return plan, false, err
			}
			if c == nil {
				return plan, false, stateErr(errors.New("the expiry queue names a key without a credit"))
			}
			next, nextC, inactive = &expired[j], c, true
		}
		if next == nil {
			break // both queues are empty
		}
		mass := StorageMassC / uint64(nextC.Amount)
		if mass > budget {
			break
		}
		budget -= mass
		plan.payouts = append(plan.payouts, TxOut{Value: nextC.Amount, ScriptVersion: ScriptVersionBasic,
			Script: append([]byte(nil), next.Payout[:]...)})
		plan.paid = append(plan.paid, next.Payout)
		plan.inactive = append(plan.inactive, inactive)
		if inactive {
			j++
		} else {
			i++
		}
	}
	return plan, false, nil
}

// CreditPayouts are the outputs the coinbase of the block at `height` must pay after the dev fee,
// in this order: the front of the payout queue of the state `v` it builds on (SHAREPOOL.md §5.2).
// The credits that became payable first go first, a tie broken by the payout program; the coinbase
// pays for as long as MaxPayoutsPerBlock and the storage mass reserve last.
func CreditPayouts(height uint64, v CreditView) ([]TxOut, error) {
	plan, err := planPayouts(height, newCreditReader(v))
	if err != nil {
		return nil, err
	}
	return plan.payouts, nil
}

// CreditStep is the one implementation of what a block at `height`, with its own share `commit`,
// its settlement `s` and `fees` in transaction fees, does to the credit state `v` of its parent:
//
//  1. the coinbase pays the front of the payout queue (CreditPayouts),
//  2. inactive keys below the dust limit return their credit to the pot,
//  3. the block's reward less the dev fee, and its fees, go into the pot,
//  4. every share unit the block settles is credited pot / PotPriceDivisor, taken from the pot.
//
// It reads only what it changes, and the root after it from the tree's stored nodes.
func CreditStep(height uint64, commit *ShareCommit, s *Settlement, fees int64, v CreditView) (*CreditStepResult, error) {
	r := newCreditReader(v)
	pot := v.Pot()

	// 1 + 2.
	plan, err := planPayouts(height, r)
	if err != nil {
		return nil, err
	}
	for _, p := range plan.dust {
		c, err := r.get(p)
		if err != nil {
			return nil, err
		}
		if c != nil {
			pot += c.Amount
		}
		r.now[p] = nil
	}
	for i, p := range plan.paid {
		if plan.inactive[i] {
			r.now[p] = nil
			continue
		}
		c, err := r.get(p)
		if err != nil {
			return nil, err
		}
		paid := *c
		paid.Amount, paid.PaidAt, paid.ReadyAt = 0, height, 0
		r.now[p] = &paid
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
		cur, err := r.get(p)
		if err != nil {
			return nil, err
		}
		var c Credit
		if cur != nil {
			c = *cur
		}
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
		r.now[p] = &c
	}

	res := &CreditStepResult{Payouts: payouts, Changed: make(map[[32]byte]*Credit, len(r.now)),
		Before: make(map[[32]byte]*Credit, len(r.now)), Pot: pot}
	for p, c := range r.now {
		orig, err := r.original(p)
		if err != nil {
			return nil, err
		}
		if c == nil && orig == nil {
			continue // created and deleted within the block — never there
		}
		res.Changed[p] = c
		res.Before[p] = orig
	}
	tree, nodes, err := creditTreeUpdate(v, res.Changed)
	if err != nil {
		return nil, err
	}
	res.Nodes = nodes
	res.Root = creditsRootFromTree(pot, tree)
	return res, nil
}

// CreditsRoot is the commitment to a credit state: TaggedHash(credits, pot ‖ tree root), where the
// tree is a sparse Merkle tree keyed by the payout program, bit by bit from the most significant.
// An empty subtree is all zeros, a subtree holding one entry is that entry's leaf, any other is
// TaggedHash(node, left ‖ right). A wallet proves its credit with the siblings on its path.
func CreditsRoot(pot int64, state map[[32]byte]Credit) [32]byte {
	keys := make([][32]byte, 0, len(state))
	for p := range state {
		keys = append(keys, p)
	}
	slices.SortFunc(keys, func(a, b [32]byte) int { return bytes.Compare(a[:], b[:]) })
	leaves := make([][32]byte, len(keys))
	for i, p := range keys {
		leaves[i] = CreditLeaf(p, state[p])
	}
	return creditsRootFromTree(pot, creditSubtree(keys, leaves, 0))
}

func creditsRootFromTree(pot int64, tree [32]byte) [32]byte {
	var buf [8 + 32]byte
	binary.LittleEndian.PutUint64(buf[0:8], uint64(pot))
	copy(buf[8:], tree[:])
	return mh.TaggedHash(creditsRootTag, buf[:])
}

// CreditLeaf is an entry's leaf: TaggedHash(leaf, payout ‖ credit).
func CreditLeaf(payout [32]byte, c Credit) [32]byte {
	ser := c.Serialize()
	var buf [32 + CreditBytes]byte
	copy(buf[:32], payout[:])
	copy(buf[32:], ser[:])
	return mh.TaggedHash(creditLeafTag, buf[:])
}

// CreditNode joins two subtrees.
func CreditNode(left, right [32]byte) [32]byte {
	var buf [64]byte
	copy(buf[:32], left[:])
	copy(buf[32:], right[:])
	return mh.TaggedHash(creditNodeTag, buf[:])
}

// creditSubtree is the root over sorted keys that agree on their first `depth` bits.
func creditSubtree(keys [][32]byte, leaves [][32]byte, depth int) [32]byte {
	return creditSubtreeEmit(keys, leaves, depth, [32]byte{}, nil)
}

// creditSubtreeEmit is creditSubtree that also hands every inner node to `emit` (nil = nobody):
// the nodes a store keeps (credittree.go). `prefix` is the keys' first `depth` bits.
func creditSubtreeEmit(keys [][32]byte, leaves [][32]byte, depth int, prefix [32]byte,
	emit func(CreditNodeKey, [32]byte)) [32]byte {
	switch len(keys) {
	case 0:
		return [32]byte{}
	case 1:
		return leaves[0]
	}
	// The keys are sorted, so those with the bit clear come first.
	split, _ := slices.BinarySearchFunc(keys, depth, func(k [32]byte, d int) int {
		if k[d/8]>>(7-d%8)&1 == 0 {
			return -1
		}
		return 1
	})
	h := CreditNode(creditSubtreeEmit(keys[:split], leaves[:split], depth+1, prefix, emit),
		creditSubtreeEmit(keys[split:], leaves[split:], depth+1, withCreditBit(prefix, depth), emit))
	if emit != nil {
		emit(CreditNodeKey{uint8(depth), prefix}, h)
	}
	return h
}
