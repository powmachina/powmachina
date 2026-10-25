package chain

import (
	"errors"
	"sync"
)

// The UTXO set — what can be spent.
//
// It is the only part of the state that has to be in memory (blocks can be pruned, the UTXO set
// cannot), so its size is considered in every decision about the format (SPEC §2.3, the dust limit).

// Outpoint points at a particular output of a particular transaction.
type Outpoint struct {
	TxID  [32]byte
	Index uint32
}

// Coin is an unspent output together with what is needed to verify it.
type Coin struct {
	Value         int64
	ScriptVersion uint8
	Script        []byte
	Height        uint64 // the height of the block it was created in
	Coinbase      bool   // a coinbase can only be spent after CoinbaseMaturity blocks
}

var (
	ErrCoinMissing  = errors.New("chain: the input does not exist or is already spent")
	ErrCoinImmature = errors.New("chain: the coinbase has not matured yet")
	ErrCoinExists   = errors.New("chain: an output with this outpoint already exists")
)

// CoinView is reading the UTXO set. Validation needs nothing more, so it can verify against
// memory the same way as against disk.
type CoinView interface {
	Get(op Outpoint) (Coin, bool)
}

// CoinScanner walks the whole UTXO set. The wallet needs it, as it has to find its own coins. It
// is a separate interface because scanning the whole set is expensive — whoever does not need it
// is fine with CoinView.
type CoinScanner interface {
	ForEachCoin(fn func(Outpoint, Coin) error) error
}

// StateView is the state a block is validated against: the coins and the credits its parent left.
type StateView interface {
	CoinView
	CreditView
}

// CoinBatch is a change to the UTXO set that is committed at once. A read has to see the writes
// from the same batch too — a transaction may spend an output of a previous one in the same block.
type CoinBatch interface {
	CoinView
	Add(op Outpoint, c Coin)
	Spend(op Outpoint)
}

// StateBatch is a change to the whole state — coins and credits — committed at once. Its reads
// see its own writes.
type StateBatch interface {
	CoinBatch
	CreditView
	// PutCredit writes an entry, nil deletes it.
	PutCredit(payout [32]byte, c *Credit)
	// PutPot writes the pot and the root of the credit state it belongs to.
	PutPot(pot int64, root [32]byte)
}

// UTXOSet is the whole state in memory — the coins and the credits — safe for concurrent use.
type UTXOSet struct {
	mu      sync.RWMutex
	coins   map[Outpoint]Coin
	credits map[[32]byte]Credit
	pot     int64
	root    [32]byte
}

func NewUTXOSet() *UTXOSet {
	return &UTXOSet{coins: make(map[Outpoint]Coin), credits: make(map[[32]byte]Credit),
		root: CreditsRoot(0, nil)}
}

func (u *UTXOSet) Pot() int64 {
	u.mu.RLock()
	defer u.mu.RUnlock()
	return u.pot
}

func (u *UTXOSet) CreditsRoot() [32]byte {
	u.mu.RLock()
	defer u.mu.RUnlock()
	return u.root
}

func (u *UTXOSet) ForEachCredit(fn func([32]byte, Credit) error) error {
	u.mu.RLock()
	defer u.mu.RUnlock()
	for p, c := range u.credits {
		if err := fn(p, c); err != nil {
			return err
		}
	}
	return nil
}

// Credit reads one key's entry.
func (u *UTXOSet) Credit(payout [32]byte) (Credit, bool) {
	u.mu.RLock()
	defer u.mu.RUnlock()
	c, ok := u.credits[payout]
	return c, ok
}

func (u *UTXOSet) Get(op Outpoint) (Coin, bool) {
	u.mu.RLock()
	defer u.mu.RUnlock()
	c, ok := u.coins[op]
	return c, ok
}

func (u *UTXOSet) Len() int {
	u.mu.RLock()
	defer u.mu.RUnlock()
	return len(u.coins)
}

// UndoEntry holds what was spent — without it a reorg cannot be rolled back.
type UndoEntry struct {
	Outpoint Outpoint
	Coin     Coin
}

// BlockUndo is everything needed to roll one block back.
type BlockUndo struct {
	Height  uint64
	Spent   []UndoEntry
	Created []Outpoint
	// Credits are the entries the block changed as they were before it, nil for one it created.
	Credits map[[32]byte]*Credit
	// Pot and CreditsRoot are the pot and the credit root before the block.
	Pot         int64
	CreditsRoot [32]byte
}

// Apply writes a block's changes into the UTXO set. It assumes the block has already passed
// validation (ValidateBlock) — here only the state changes.
func (u *UTXOSet) Apply(b *Block) (*BlockUndo, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	batch := mapBatch{u}
	undo, err := ApplyBlockTo(b, batch)
	if err != nil {
		u.rollbackLocked(undo)
		return nil, err
	}
	return undo, nil
}

// mapBatch is a StateBatch over the in-memory set, under its lock. Writes go straight into it, so
// "committing" is a no-op and rolling changes back is done by rollbackLocked.
type mapBatch struct{ u *UTXOSet }

func (m mapBatch) Get(op Outpoint) (Coin, bool) { c, ok := m.u.coins[op]; return c, ok }
func (m mapBatch) Add(op Outpoint, c Coin)      { m.u.coins[op] = c }
func (m mapBatch) Spend(op Outpoint)            { delete(m.u.coins, op) }
func (m mapBatch) Pot() int64                   { return m.u.pot }
func (m mapBatch) CreditsRoot() [32]byte        { return m.u.root }
func (m mapBatch) ForEachCredit(fn func([32]byte, Credit) error) error {
	for p, c := range m.u.credits {
		if err := fn(p, c); err != nil {
			return err
		}
	}
	return nil
}
func (m mapBatch) PutCredit(p [32]byte, c *Credit) {
	if c == nil {
		delete(m.u.credits, p)
	} else {
		m.u.credits[p] = *c
	}
}
func (m mapBatch) PutPot(pot int64, root [32]byte) { m.u.pot, m.u.root = pot, root }

// ApplyBlockTo is the **only implementation** of what a block does to the UTXO set. Both the
// in-memory set and persistent storage use it through CoinBatch — two implementations of this
// logic would be exactly the kind of difference that splits a network (project rule 1: the PoW has exactly one implementation).
//
// It assumes the block has already passed validation. On an error it returns the undo filled up to
// the point of failure, so whatever was already written can be rolled back.
func ApplyBlockTo(b *Block, batch StateBatch) (*BlockUndo, error) {
	height := uint64(b.Header.Height)
	undo := &BlockUndo{Height: height, Pot: batch.Pot(), CreditsRoot: batch.CreditsRoot()}
	var fees int64

	for _, tx := range b.Txs {
		if !tx.IsCoinbase() {
			for _, in := range tx.Inputs {
				op := Outpoint{TxID: in.PrevTxID, Index: in.PrevIndex}
				coin, ok := batch.Get(op)
				if !ok {
					return undo, ErrCoinMissing
				}
				undo.Spent = append(undo.Spent, UndoEntry{Outpoint: op, Coin: coin})
				batch.Spend(op)
				fees += coin.Value
			}
			for _, out := range tx.Outputs {
				fees -= out.Value
			}
		}
		txid := tx.TxID()
		for i, out := range tx.Outputs {
			if out.ScriptVersion == ScriptVersionData {
				continue // a data output is unspendable, it does not belong in the UTXO set
			}
			op := Outpoint{TxID: txid, Index: uint32(i)}
			if _, exists := batch.Get(op); exists {
				return undo, ErrCoinExists
			}
			batch.Add(op, Coin{
				Value: out.Value, ScriptVersion: out.ScriptVersion, Script: out.Script,
				Height: height, Coinbase: tx.IsCoinbase(),
			})
			undo.Created = append(undo.Created, op)
		}
	}
	if height == 0 {
		return undo, nil // the genesis settles nothing: the credit state starts empty
	}
	step, err := CreditStep(height, &b.Share, &b.Settlement, fees, batch)
	if err != nil {
		return undo, err
	}
	undo.Credits = step.Before
	for p, c := range step.Changed {
		batch.PutCredit(p, c)
	}
	batch.PutPot(step.Pot, step.Root)
	return undo, nil
}

// Undo restores the state from before the block. Used during a reorg.
func (u *UTXOSet) Undo(undo *BlockUndo) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.rollbackLocked(undo)
}

// The spent coins come back first and the created ones go after: a transaction may spend an
// output of an earlier one in the same block, and that output is in both lists. Restored and then
// deleted it is gone, as it was before the block; the other way round it would come back as a
// coin that never existed (audit 2026-10-25, PM-01).
func (u *UTXOSet) rollbackLocked(undo *BlockUndo) {
	for _, e := range undo.Spent {
		u.coins[e.Outpoint] = e.Coin
	}
	for _, op := range undo.Created {
		delete(u.coins, op)
	}
	for p, c := range undo.Credits {
		mapBatch{u}.PutCredit(p, c)
	}
	u.pot, u.root = undo.Pot, undo.CreditsRoot
}

// AddCoin is a helper for setting up an initial state (tests, the genesis).
func (u *UTXOSet) AddCoin(op Outpoint, c Coin) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.coins[op] = c
}

// Snapshot returns a copy — for comparing state in tests.
// ForEachCoin also makes UTXOSet a CoinScanner.
func (u *UTXOSet) ForEachCoin(fn func(Outpoint, Coin) error) error {
	u.mu.RLock()
	defer u.mu.RUnlock()
	for op, c := range u.coins {
		if err := fn(op, c); err != nil {
			return err
		}
	}
	return nil
}

func (u *UTXOSet) Snapshot() map[Outpoint]Coin {
	u.mu.RLock()
	defer u.mu.RUnlock()
	out := make(map[Outpoint]Coin, len(u.coins))
	for k, v := range u.coins {
		out[k] = v
	}
	return out
}
