package chain

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"
)

// Mempool — the transactions waiting to be put into a block.
//
// The rules here are **policy, not consensus**: a node may have them stricter than the network.
// That is exactly why the dust limit and the minimum fee belong here — they keep the UTXO set
// small and spam expensive, but they do not decide a block's validity (SPEC §2.3).

const (
	// MinRelayFeePerKB is the minimum fee per kilobyte (SPEC §2).
	MinRelayFeePerKB int64 = 1000
	// DustLimit is the smallest sensible output value. Smaller outputs cost more in the UTXO set
	// than they carry themselves.
	DustLimit int64 = 10_000
	// PolicyStorageMassC is the storage-mass constant **for computing the fee** — node policy, not
	// consensus. At launch it is stricter (10⁹) than consensus (10⁸), while POWM is cheap and spam
	// is cheap in dollars; it is lowered by a node release without a fork, the same as the fee
	// floor (the fees-and-dust note, R2). The block cap is always computed with the consensus StorageMassC.
	PolicyStorageMassC uint64 = 1_000_000_000
	// MaxMempoolBytes is the cap on the mempool's memory.
	MaxMempoolBytes = 64 * 1024 * 1024
	// MaxMempoolTxs is the cap on the number of transactions.
	MaxMempoolTxs = 20_000
	// MaxChain is the cap on chaining unconfirmed transactions: a transaction may have at most 24
	// ancestors in the pool (25 including itself) and no transaction in the pool may have more than
	// 24 descendants. Like bitcoind. Without a cap, a chain of thousands of transactions would make
	// every removal and every block selection expensive.
	MaxChain = 25
)

var (
	ErrMempoolDuplicate = errors.New("mempool: we already have the transaction")
	ErrMempoolConflict  = errors.New("mempool: the input is already spent by another transaction")
	ErrFeeTooLow        = errors.New("mempool: the fee is below the minimum")
	ErrDustOutput       = errors.New("mempool: the output is below the dust limit")
	ErrMempoolFull      = errors.New("mempool: it is full and the transaction pays less than the others")
	ErrCoinbaseInPool   = errors.New("mempool: a coinbase does not belong in the mempool")
	ErrTxStorageMass    = errors.New("mempool: the transaction creates too many tiny outputs — it does not fit into a block")
	ErrChainTooLong     = errors.New("mempool: too many unconfirmed transactions in the chain (max 25) — wait for a block")
)

// MissingInputs is the rejection for missing inputs that also carries their list.
//
// It is the only rejection worth retrying: the others (a bad signature, a low fee, a conflict) do
// not change on a second attempt. That is why it can be recognized with `errors.As` while
// `errors.Is(err, ErrCoinMissing)` still holds — a caller without an orphan pool sees it as before.
type MissingInputs struct {
	Outpoints []Outpoint
}

func (e *MissingInputs) Error() string {
	return fmt.Sprintf("%v (%d inputs)", ErrCoinMissing, len(e.Outpoints))
}

func (e *MissingInputs) Unwrap() error { return ErrCoinMissing }

// MempoolEntry is a transaction together with what we know about it.
type MempoolEntry struct {
	Tx      *Tx
	TxID    [32]byte
	Fee     int64
	Size    int
	Added   time.Time
	FeeRate int64 // cogs per 1 000 units of weight (weight = max(bytes, mass per policy))
	// Mass is the storage mass per consensus — for the block cap in a template.
	Mass uint64
	// Weight is the weight for the fee (FeeRate = Fee×1000/Weight).
	Weight uint64
}

// PolicyWeight is a transaction's weight for the fee: the larger of its bytes and its storage mass
// per policy. An ordinary payment has a weight equal to its bytes; tiny outputs raise it. The
// wallet computes the fee the same way, which is why this is one function for both.
func PolicyWeight(size int, inValues []int64, outs []TxOut) uint64 {
	w := uint64(size)
	if m := StorageMass(inValues, outs, PolicyStorageMassC); m > w {
		w = m
	}
	return w
}

type Mempool struct {
	mu      sync.RWMutex
	params  NetParams
	entries map[[32]byte]*MempoolEntry
	spent   map[Outpoint][32]byte // outpoint → the txid that spends it
	bytes   int
}

func NewMempool(params NetParams) *Mempool {
	return &Mempool{
		params:  params,
		entries: make(map[[32]byte]*MempoolEntry),
		spent:   make(map[Outpoint][32]byte),
	}
}

// Accept verifies a transaction and files it. `utxo` is the confirmed state; a transaction may
// also spend outputs of other transactions from the mempool (chaining).
func (m *Mempool) Accept(tx *Tx, utxo CoinView, height uint64) (*MempoolEntry, error) {
	if tx.IsCoinbase() {
		return nil, ErrCoinbaseInPool
	}
	if err := tx.CheckSanity(); err != nil {
		return nil, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	txid := tx.TxID()
	if _, dup := m.entries[txid]; dup {
		return nil, ErrMempoolDuplicate
	}

	// The inputs: they have to exist and must not be spent by another transaction in the pool.
	//
	// Missing inputs are **all collected**, not just the first: the caller builds an orphan entry
	// from them and asks for the parents, so it needs the whole list and not the first one found.
	coins := make([]Coin, 0, len(tx.Inputs))
	var missing []Outpoint
	var inSum int64
	for _, in := range tx.Inputs {
		op := Outpoint{TxID: in.PrevTxID, Index: in.PrevIndex}
		if other, conflict := m.spent[op]; conflict {
			return nil, fmt.Errorf("%w (%x)", ErrMempoolConflict, other[:6])
		}
		coin, ok := utxo.Get(op)
		if !ok {
			// It may be the output of a transaction that is also in the pool.
			parent, inPool := m.entries[op.TxID]
			switch {
			case !inPool || int(op.Index) >= len(parent.Tx.Outputs):
				missing = append(missing, op)
				continue
			case parent.Tx.Outputs[op.Index].ScriptVersion == ScriptVersionData:
				// A data output cannot be spent and never will be — this is not an orphan.
				return nil, ErrCoinMissing
			}
			out := parent.Tx.Outputs[op.Index]
			coin = Coin{Value: out.Value, ScriptVersion: out.ScriptVersion, Script: out.Script,
				Height: height}
		}
		if coin.Coinbase && height < coin.Height+CoinbaseMaturity {
			return nil, ErrCoinImmature
		}
		coins = append(coins, coin)
		inSum += coin.Value
		if inSum < 0 || inSum > MaxMoney {
			return nil, ErrTxBadValue
		}
	}
	if len(missing) > 0 {
		return nil, &MissingInputs{Outpoints: missing}
	}

	var outSum int64
	for _, o := range tx.Outputs {
		if o.ScriptVersion != ScriptVersionData && o.Value < DustLimit {
			return nil, fmt.Errorf("%w: %d cogs (minimum %d)", ErrDustOutput, o.Value, DustLimit)
		}
		outSum += o.Value
	}
	if outSum > inSum {
		return nil, ErrInputsBelowOutput
	}

	// The signatures last — they are the most expensive.
	for i := range tx.Inputs {
		if err := VerifyInput(tx, i, coins); err != nil {
			return nil, fmt.Errorf("input %d: %w", i, err)
		}
	}

	size := len(tx.Serialize())
	fee := inSum - outSum
	values := make([]int64, len(coins))
	for i, c := range coins {
		values[i] = c.Value
	}
	// A transaction that would not fit into a block even on its own has no business in the mempool.
	mass := TxStorageMass(values, tx)
	if mass > MaxBlockStorageMass-CoinbaseStorageMassReserve {
		return nil, fmt.Errorf("%w (mass %d, cap %d)", ErrTxStorageMass, mass,
			MaxBlockStorageMass-CoinbaseStorageMassReserve)
	}
	weight := PolicyWeight(size, values, tx.Outputs)
	if weight > uint64(math.MaxInt64/1000) {
		return nil, ErrTxStorageMass
	}
	feeRate := fee * 1000 / int64(weight)
	if feeRate < MinRelayFeePerKB {
		return nil, fmt.Errorf("%w: %d cogs per 1 000 units of weight (minimum %d, weight %d)",
			ErrFeeTooLow, feeRate, MinRelayFeePerKB, weight)
	}

	// Chaining: at most MaxChain-1 ancestors in the pool and no ancestor may exceed MaxChain-1
	// descendants.
	ancestors := m.ancestorsLocked(tx)
	if len(ancestors)+1 > MaxChain {
		return nil, fmt.Errorf("%w (%d ancestors)", ErrChainTooLong, len(ancestors))
	}
	for a := range ancestors {
		if len(m.descendantsLocked(a))+1 > MaxChain-1 {
			return nil, fmt.Errorf("%w (ancestor %x has too many descendants)", ErrChainTooLong, a[:6])
		}
	}

	if m.bytes+size > MaxMempoolBytes || len(m.entries) >= MaxMempoolTxs {
		if !m.evictForLocked(feeRate, size, ancestors) {
			return nil, ErrMempoolFull
		}
	}

	e := &MempoolEntry{Tx: tx, TxID: txid, Fee: fee, Size: size, Added: time.Now(), FeeRate: feeRate,
		Mass: mass, Weight: weight}
	m.entries[txid] = e
	m.bytes += size
	for _, in := range tx.Inputs {
		m.spent[Outpoint{TxID: in.PrevTxID, Index: in.PrevIndex}] = txid
	}
	return e, nil
}

// ancestorsLocked returns the transaction's ancestors that are in the pool (excluding itself).
func (m *Mempool) ancestorsLocked(tx *Tx) map[[32]byte]bool {
	out := make(map[[32]byte]bool)
	queue := []*Tx{tx}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, in := range cur.Inputs {
			parent, ok := m.entries[in.PrevTxID]
			if !ok || out[in.PrevTxID] {
				continue
			}
			out[in.PrevTxID] = true
			if len(out) >= MaxChain { // no need to count further — it is already over the cap
				return out
			}
			queue = append(queue, parent.Tx)
		}
	}
	return out
}

// descendantsLocked returns a transaction's descendants in the pool (excluding itself).
func (m *Mempool) descendantsLocked(id [32]byte) map[[32]byte]bool {
	out := make(map[[32]byte]bool)
	queue := [][32]byte{id}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		e, ok := m.entries[cur]
		if !ok {
			continue
		}
		for i := range e.Tx.Outputs {
			child, spent := m.spent[Outpoint{TxID: cur, Index: uint32(i)}]
			if !spent || out[child] {
				continue
			}
			out[child] = true
			queue = append(queue, child)
		}
	}
	return out
}

// removeTreeLocked throws out a transaction and all its descendants — a child without its parent
// is invalid and in a block template it would make the whole block invalid.
func (m *Mempool) removeTreeLocked(id [32]byte) {
	for d := range m.descendantsLocked(id) {
		m.removeLocked(d)
	}
	m.removeLocked(id)
}

// evictForLocked frees space by throwing out the cheapest transactions (each with its
// descendants). It returns false if that would throw out something more expensive than the
// incoming transaction, or one of its own ancestors — then the new one is rejected instead.
func (m *Mempool) evictForLocked(feeRate int64, size int, protect map[[32]byte]bool) bool {
	type cand struct {
		id   [32]byte
		rate int64
	}
	list := make([]cand, 0, len(m.entries))
	for id, e := range m.entries {
		list = append(list, cand{id, e.FeeRate})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].rate < list[j].rate })

	freed := 0
	victims := make(map[[32]byte]bool)
	fits := func() bool {
		return m.bytes-freed+size <= MaxMempoolBytes && len(m.entries)-len(victims) < MaxMempoolTxs
	}
	for _, c := range list {
		if fits() {
			break
		}
		if c.rate >= feeRate {
			break // everything beyond this is more expensive
		}
		if victims[c.id] {
			continue
		}
		pkg := m.descendantsLocked(c.id)
		pkg[c.id] = true
		ok := true
		for id := range pkg {
			if protect[id] || m.entries[id].FeeRate >= feeRate {
				ok = false // a descendant pays more than the new transaction, or it is its ancestor
				break
			}
		}
		if !ok {
			continue
		}
		for id := range pkg {
			if !victims[id] {
				victims[id] = true
				freed += m.entries[id].Size
			}
		}
	}
	if !fits() {
		return false
	}
	for id := range victims {
		m.removeLocked(id)
	}
	return true
}

func (m *Mempool) removeLocked(id [32]byte) {
	e, ok := m.entries[id]
	if !ok {
		return
	}
	for _, in := range e.Tx.Inputs {
		delete(m.spent, Outpoint{TxID: in.PrevTxID, Index: in.PrevIndex})
	}
	m.bytes -= e.Size
	delete(m.entries, id)
}

// RemoveConfirmed throws out the transactions that made it into a block, and those conflicting with them.
func (m *Mempool) RemoveConfirmed(b *Block) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, tx := range b.Txs {
		// A confirmed transaction leaves on its own — its children stay valid, their inputs are now
		// in the UTXO set.
		m.removeLocked(tx.TxID())
		for _, in := range tx.Inputs {
			op := Outpoint{TxID: in.PrevTxID, Index: in.PrevIndex}
			if other, ok := m.spent[op]; ok {
				m.removeTreeLocked(other) // a conflict with what was confirmed — with its descendants too
			}
		}
	}
}

// Select picks transactions for a block template in packages: a transaction together with the
// ancestors that are not selected yet, ordered by the rate of the whole package. A child with a
// high fee therefore pulls a cheap parent along (CPFP) and the parent always comes before the
// child in the block. `limit` is the room in bytes and `massLimit` the room in storage mass left
// after the coinbase.
func (m *Mempool) Select(limit int, massLimit uint64) []*Tx {
	m.mu.RLock()
	defer m.mu.RUnlock()

	type pkg struct {
		e    *MempoolEntry
		anc  map[[32]byte]bool
		rate int64
	}
	list := make([]pkg, 0, len(m.entries))
	for _, e := range m.entries {
		anc := m.ancestorsLocked(e.Tx)
		fee, weight := e.Fee, e.Weight
		for a := range anc {
			fee += m.entries[a].Fee
			weight += m.entries[a].Weight
		}
		list = append(list, pkg{e, anc, fee * 1000 / int64(max(weight, 1))})
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].rate != list[j].rate {
			return list[i].rate > list[j].rate
		}
		if !list[i].e.Added.Equal(list[j].e.Added) {
			return list[i].e.Added.Before(list[j].e.Added)
		}
		return string(list[i].e.TxID[:]) < string(list[j].e.TxID[:])
	})

	out := make([]*Tx, 0, len(list))
	used := 0
	var usedMass uint64
	inBlock := make(map[[32]byte]bool)
	for _, p := range list {
		if inBlock[p.e.TxID] {
			continue
		}
		// The group = unselected ancestors + the transaction, in parent-before-child order.
		group := []*MempoolEntry{}
		for a := range p.anc {
			if !inBlock[a] {
				group = append(group, m.entries[a])
			}
		}
		group = append(group, p.e)
		size, mass := 0, uint64(0)
		for _, g := range group {
			size += g.Size
			mass += g.Mass
		}
		if used+size > limit || usedMass+mass > massLimit {
			continue
		}
		for _, g := range m.topoLocked(group) {
			out = append(out, g.Tx)
			inBlock[g.TxID] = true
		}
		used += size
		usedMass += mass
	}
	return out
}

// topoLocked orders a group so that every parent comes before its child.
func (m *Mempool) topoLocked(group []*MempoolEntry) []*MempoolEntry {
	in := make(map[[32]byte]*MempoolEntry, len(group))
	for _, g := range group {
		in[g.TxID] = g
	}
	var out []*MempoolEntry
	done := make(map[[32]byte]bool, len(group))
	var visit func(g *MempoolEntry)
	visit = func(g *MempoolEntry) {
		if done[g.TxID] {
			return
		}
		done[g.TxID] = true
		for _, input := range g.Tx.Inputs {
			if parent, ok := in[input.PrevTxID]; ok {
				visit(parent)
			}
		}
		out = append(out, g)
	}
	sort.Slice(group, func(i, j int) bool { return string(group[i].TxID[:]) < string(group[j].TxID[:]) })
	for _, g := range group {
		visit(g)
	}
	return out
}

func (m *Mempool) Len() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.entries)
}

func (m *Mempool) Bytes() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.bytes
}

func (m *Mempool) Get(id [32]byte) (*MempoolEntry, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	e, ok := m.entries[id]
	return e, ok
}

// IDs returns the list of transactions in the pool.
func (m *Mempool) IDs() [][32]byte {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([][32]byte, 0, len(m.entries))
	for id := range m.entries {
		out = append(out, id)
	}
	return out
}
