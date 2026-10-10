package wallet

import (
	"errors"
	"fmt"
	"time"

	"powmachina/internal/chain"
)

// Node is what the wallet needs from a node. The RPC client implements it
// (internal/nodeclient); in tests a fake in-memory node does.
type Node interface {
	// Tip returns the height and the ID of the best block.
	Tip() (uint64, [32]byte, error)
	// Blocks returns the blocks from height `from` (at most `count`) on the active branch.
	Blocks(from uint64, count int) ([]*chain.Block, error)
	// Mempool returns the IDs of the transactions the node holds.
	Mempool() ([][32]byte, error)
	// Broadcast sends a transaction to the network.
	Broadcast(tx *chain.Tx) error
}

// SyncBatch is the number of blocks per request.
const SyncBatch = 100

// ErrReorgTooDeep: the node is on a branch that forked from ours deeper than we remember blocks.
// In that case the wallet rescans from its birth.
var ErrReorgTooDeep = errors.New("wallet: reorganization deeper than the wallet remembers — rescanning")

// SyncResult says what the sync found.
type SyncResult struct {
	From, To   uint64
	Received   []*Tx  // new incoming transactions
	RolledBack uint64 // how many blocks the wallet rolled back during a reorg (0 = there was none)
}

// Sync brings the wallet up to the node's tip. It saves as it goes, so an interruption loses nothing.
func (w *Wallet) Sync(node Node) (*SyncResult, error) {
	w.syncMu.Lock()
	defer w.syncMu.Unlock()
	res := &SyncResult{}
	w.mu.Lock()
	res.From = w.height
	w.mu.Unlock()
	for attempt := 0; ; attempt++ {
		if attempt > 10 {
			return res, errors.New("wallet: the chain changes faster than it can be scanned")
		}
		tipH, _, err := node.Tip()
		if err != nil {
			return res, err
		}
		if err := w.checkReorg(node, res); err != nil {
			return res, err
		}
		done, err := w.pull(node, tipH, res)
		if err != nil {
			return res, err
		}
		if done {
			break
		}
	}
	if err := w.pending(node); err != nil {
		return res, err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	res.To = w.height
	return res, w.saveLocked()
}

// checkReorg verifies that our last block is still on the node's active branch. If it is not, it finds
// the common ancestor in `recent` and goes back to it.
func (w *Wallet) checkReorg(node Node, res *SyncResult) error {
	w.mu.Lock()
	synced, height, tip := w.synced, w.height, w.tip
	recent := append([]blockRef{}, w.recent...)
	w.mu.Unlock()
	if !synced {
		return nil
	}
	same := func(h uint64, id [32]byte) (bool, error) {
		bs, err := node.Blocks(h, 1)
		if err != nil {
			return false, err
		}
		return len(bs) == 1 && bs[0].Header.ID() == id, nil
	}
	ok, err := same(height, tip)
	if err != nil || ok {
		return err
	}
	for i := len(recent) - 1; i >= 0; i-- {
		ok, err := same(recent[i].Height, recent[i].ID)
		if err != nil {
			return err
		}
		if ok {
			w.mu.Lock()
			res.RolledBack += w.height - recent[i].Height
			w.rollbackLocked(recent[i].Height)
			w.mu.Unlock()
			return nil
		}
	}
	w.mu.Lock()
	w.resetLocked()
	w.mu.Unlock()
	return nil
}

// pull downloads and applies the blocks after `tipH`. It returns done=false when it hits a block that
// does not link up (a reorg during the scan) — Sync tries again.
func (w *Wallet) pull(node Node, tipH uint64, res *SyncResult) (bool, error) {
	for {
		w.mu.Lock()
		from := w.height + 1
		if !w.synced {
			from = w.scanStartLocked()
		}
		w.mu.Unlock()
		if from > tipH {
			return true, nil
		}
		count := SyncBatch
		if left := tipH - from + 1; left < uint64(count) {
			count = int(left)
		}
		blocks, err := node.Blocks(from, count)
		if err != nil {
			return false, err
		}
		if len(blocks) == 0 {
			return true, nil
		}
		w.mu.Lock()
		for i, b := range blocks {
			h := from + uint64(i)
			if uint64(b.Header.Height) != h {
				w.mu.Unlock()
				return false, fmt.Errorf("wallet: node sent block %d instead of %d", b.Header.Height, h)
			}
			if w.synced && b.Header.PrevBlock != w.tip {
				w.mu.Unlock()
				return false, nil
			}
			res.Received = append(res.Received, w.applyLocked(b)...)
		}
		err = w.saveLocked()
		w.mu.Unlock()
		if err != nil {
			return false, err
		}
	}
}

// scanStartLocked is the height to scan a not-yet-synced wallet from: the earliest birth (the wallet's
// or an imported key's). Never the genesis: its coinbase cannot be spent and the node does not even
// hold its body.
func (w *Wallet) scanStartLocked() uint64 {
	start := w.birth
	for _, im := range w.imported {
		start = min(start, im.Birth)
	}
	return max(start, 1)
}

// applyLocked accounts for a block. It returns the new incoming transactions.
func (w *Wallet) applyLocked(b *chain.Block) []*Tx {
	h := uint64(b.Header.Height)
	id := b.Header.ID()
	var incoming []*Tx
	for ti, tx := range b.Txs {
		txid := tx.TxID()
		var spent int64
		allOurs := true
		for _, in := range tx.Inputs {
			op := chain.Outpoint{TxID: in.PrevTxID, Index: in.PrevIndex}
			if c, ok := w.coins[op]; ok && !c.spent() {
				c.SpentBy, c.SpentHeight, c.PendingBy = txid, h, [32]byte{}
				spent += c.Value
			} else {
				allOurs = false
			}
		}
		var received, outSum int64
		var outs []Output
		touched := spent > 0
		for oi, out := range tx.Outputs {
			outSum += out.Value
			o := Output{Index: uint32(oi), Value: out.Value}
			if out.ScriptVersion == chain.ScriptVersionBasic && len(out.Script) == chain.PubkeyBytes {
				pub := a32(out.Script)
				o.Address = w.encode(pub)
				if ref, ok := w.keys.derived[pub]; ok {
					if _, have := w.keys.secrets[pub]; have {
						o.Mine = true
						o.Change = w.isChangeLocked(ref, pub, spent > 0)
						received += out.Value
						touched = true
						op := chain.Outpoint{TxID: txid, Index: uint32(oi)}
						if c, ok := w.coins[op]; ok && c.Height == 0 {
							c.Height = h // our unconfirmed output got confirmed; the reservation stays
						} else {
							w.coins[op] = &Coin{Outpoint: op, Value: out.Value, PubKey: pub, Height: h,
								Coinbase: ti == 0, PendingBy: w.pendingSpenderLocked(op)}
						}
						w.markUsedLocked(ref)
					}
				}
			}
			outs = append(outs, o)
		}
		if !touched {
			continue
		}
		fee := int64(-1)
		if allOurs && ti != 0 && len(tx.Inputs) > 0 {
			fee = spent - outSum
		}
		prev, had := w.txs[txid]
		t := &Tx{ID: txid, Height: h, BlockID: id, Time: int64(b.Header.Time), Received: received,
			Spent: spent, Fee: fee, Coinbase: ti == 0, Outputs: outs}
		if had && prev.Raw != nil {
			t.Raw, t.Inputs = prev.Raw, prev.Inputs // ours, sent — keep it in case of a reorg
		}
		w.txs[txid] = t
		if received > spent && !had {
			incoming = append(incoming, t)
		}
	}
	w.synced, w.height, w.tip = true, h, id
	w.recent = append(w.recent, blockRef{Height: h, ID: id})
	if len(w.recent) > int(chain.ReorgLimit)+1 {
		w.recent = w.recent[len(w.recent)-int(chain.ReorgLimit)-1:]
	}
	w.pruneLocked()
	return incoming
}

// markUsedLocked moves a branch's counter when money arrived at an address beyond it — a restore from
// the words therefore also finds the addresses that were handed out before the backup.
func (w *Wallet) markUsedLocked(ref keyRef) {
	switch ref.kind {
	case branchReceive:
		if ref.index >= w.nextRecv {
			w.nextRecv = ref.index + 1
		}
	case branchChange:
		if ref.index >= w.nextChange {
			w.nextChange = ref.index + 1
		}
	default:
		return
	}
	_ = w.extendLocked()
}

// pruneLocked forgets the coins spent deeper than ReorgLimit — those cannot come back any more.
func (w *Wallet) pruneLocked() {
	if w.height <= chain.ReorgLimit {
		return
	}
	floor := w.height - chain.ReorgLimit
	for op, c := range w.coins {
		if c.spent() && c.SpentHeight < floor {
			delete(w.coins, op)
		}
	}
}

// rollbackLocked returns the wallet to height `h` (block h stays).
func (w *Wallet) rollbackLocked(h uint64) {
	for op, c := range w.coins {
		switch {
		case c.Height > h:
			delete(w.coins, op)
		case c.SpentHeight > h:
			c.SpentBy, c.SpentHeight = [32]byte{}, 0
		}
	}
	for id, t := range w.txs {
		if t.Height <= h {
			continue
		}
		if t.Raw != nil {
			t.Height, t.BlockID = 0, [32]byte{} // ours, sent: back among the unconfirmed
		} else {
			delete(w.txs, id)
		}
	}
	w.restorePendingLocked()
	for len(w.recent) > 0 && w.recent[len(w.recent)-1].Height > h {
		w.recent = w.recent[:len(w.recent)-1]
	}
	w.height = h
	if len(w.recent) > 0 {
		w.tip = w.recent[len(w.recent)-1].ID
	}
}

// resetLocked drops the cache and the wallet rescans from its birth. The keys, the labels and the
// transactions we sent stay.
func (w *Wallet) resetLocked() {
	w.synced, w.height, w.tip, w.recent = false, 0, [32]byte{}, nil
	w.coins = make(map[chain.Outpoint]*Coin)
	for id, t := range w.txs {
		if t.Raw == nil {
			delete(w.txs, id)
			continue
		}
		t.Height, t.BlockID = 0, [32]byte{}
	}
	w.restorePendingLocked()
}

// Rescan rescans the wallet from `from` (for instance after importing a key with history). The cache is
// dropped, because the coins of older keys from before `from` would otherwise be missing.
func (w *Wallet) Rescan(from uint64) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.birth = min(w.birth, from)
	w.resetLocked()
	return w.saveLocked()
}

// restorePendingLocked restores the state of our unconfirmed transactions after a reorg or a rescan:
// their own outputs as unconfirmed coins again, their inputs reserved again.
func (w *Wallet) restorePendingLocked() {
	for _, t := range w.txs {
		if t.Height != 0 || t.Raw == nil {
			continue
		}
		for _, o := range t.Outputs {
			if !o.Mine {
				continue
			}
			op := chain.Outpoint{TxID: t.ID, Index: o.Index}
			if _, ok := w.coins[op]; ok {
				continue
			}
			if pub, err := w.DecodeAddressLocked(o.Address); err == nil {
				w.coins[op] = &Coin{Outpoint: op, Value: o.Value, PubKey: pub}
			}
		}
	}
	for _, t := range w.txs {
		if t.Height != 0 || t.Raw == nil {
			continue
		}
		for _, op := range t.Inputs {
			if c, ok := w.coins[op]; ok && !c.spent() {
				c.PendingBy = t.ID
			}
		}
	}
}

// pendingSpenderLocked returns our unconfirmed transaction that spends a given output (or zero).
func (w *Wallet) pendingSpenderLocked(op chain.Outpoint) [32]byte {
	for _, t := range w.txs {
		if t.Height != 0 {
			continue
		}
		for _, in := range t.Inputs {
			if in == op {
				return t.ID
			}
		}
	}
	return [32]byte{}
}

// PendingGrace: an unconfirmed transaction the node does not have in its mempool is sent again first
// (the node may have restarted). If the node refuses it, the wallet drops it and frees the coins.
var PendingGrace = 2 * time.Minute

// pending checks our unconfirmed transactions against the node's mempool.
func (w *Wallet) pending(node Node) error {
	w.mu.Lock()
	var waiting []*Tx
	for _, t := range w.txs {
		if t.Height == 0 && t.Raw != nil && time.Since(time.Unix(t.Time, 0)) > PendingGrace {
			waiting = append(waiting, t)
		}
	}
	w.mu.Unlock()
	if len(waiting) == 0 {
		return nil
	}
	ids, err := node.Mempool()
	if err != nil {
		return err
	}
	inPool := make(map[[32]byte]bool, len(ids))
	for _, id := range ids {
		inPool[id] = true
	}
	for _, t := range waiting {
		if inPool[t.ID] {
			continue
		}
		tx, _, err := chain.DeserializeTx(t.Raw)
		if err == nil {
			err = node.Broadcast(tx)
		}
		if err != nil {
			w.mu.Lock()
			w.abandonLocked(t.ID)
			w.mu.Unlock()
		}
	}
	return nil
}

// abandonLocked drops our unconfirmed transaction, frees its coins and also drops our transactions that
// spent its (unconfirmed) outputs — without it they are invalid.
func (w *Wallet) abandonLocked(id [32]byte) {
	t, ok := w.txs[id]
	if !ok || t.Height != 0 {
		return
	}
	for _, c := range w.coins {
		if c.PendingBy == id {
			c.PendingBy = [32]byte{}
		}
	}
	delete(w.txs, id)
	for op, c := range w.coins {
		if op.TxID != id || c.Height != 0 {
			continue
		}
		child := c.PendingBy
		delete(w.coins, op)
		if child != ([32]byte{}) {
			w.abandonLocked(child)
		}
	}
}
