package store

import (
	"encoding/binary"
	"fmt"
	"math"

	"github.com/cockroachdb/pebble/v2"

	"powmachina/internal/chain"
	"powmachina/internal/mh"
)

// The address index: an address's history and its summary.
//
// Bitcoin Core **has not and never had** an address index — which is why electrs, blockbook and
// friends exist. We have it right in the node's storage, and that is the whole difference: it is
// written in the same atomic batch as the block and unwound through the undo data during a reorg. No
// difference can arise between the chain and the index — and that is exactly what breaks explorers
// built on a separate database (iquidus: "a transaction got deducted double").
//
// The key is neither the address nor the script, but the **script hash**. A script is always 32 B
// today (version 0), but versions 1–15 are reserved and may have a different length; a
// variable-length prefix would break prefix scans. A hash is a fixed 32 B and does not care what a
// soft fork brings.

const (
	// dirReceived is the record "the address received from this output".
	dirReceived byte = 0
	// dirSpent is the record "the address spent with this input".
	dirSpent byte = 1
)

// domainAddrIndex separates the script hash from the other hashes in the project.
const domainAddrIndex = "PowMachina/addr-index/v0"

// ScriptKey is an address's key in the index — a hash of the version and the script.
type ScriptKey [32]byte

// ScriptKeyFor builds the key from an output script.
func ScriptKeyFor(version uint8, script []byte) ScriptKey {
	buf := make([]byte, 0, 1+len(script))
	buf = append(buf, version)
	buf = append(buf, script...)
	return ScriptKey(mh.TaggedHash(domainAddrIndex, buf))
}

// keyAddrEntry: <prefix><scriptKey:32><height:8 BE><txid:32><dir:1><n:4>
//
// A prefix scan by scriptKey returns the history ordered by height — exactly the order an explorer
// wants to show it in, without sorting.
func keyAddrEntry(sk ScriptKey, height uint64, txid [32]byte, dir byte, n uint32) []byte {
	k := make([]byte, 1+32+8+32+1+4)
	k[0] = prefixAddr
	copy(k[1:], sk[:])
	binary.BigEndian.PutUint64(k[33:], height)
	copy(k[41:], txid[:])
	k[73] = dir
	binary.BigEndian.PutUint32(k[74:], n)
	return k
}

func keyAddrPrefix(sk ScriptKey) []byte {
	k := make([]byte, 1+32)
	k[0] = prefixAddr
	copy(k[1:], sk[:])
	return k
}

func keyAddrSummary(sk ScriptKey) []byte {
	k := make([]byte, 1+32)
	k[0] = prefixAddrSum
	copy(k[1:], sk[:])
	return k
}

// AddrEntry is one movement on an address.
type AddrEntry struct {
	Height uint64
	TxID   [32]byte
	// Received says whether the address was receiving (an output) or spending (an input).
	Received bool
	// N is the output index on a receive, the input index on a spend.
	N     uint32
	Value int64
}

// AddrSummary is an address's summary. It is kept precomputed, so the balance does not cost a walk through the history.
type AddrSummary struct {
	Received int64
	Sent     int64
	// TxCount is the number of **transactions** that touched the address, not the number of movements.
	TxCount uint32
	// Script is the original output script — the index keeps it so an address can be built from it
	// without having to look the output up.
	ScriptVersion uint8
	Script        []byte
}

// Balance is an address's balance.
func (s AddrSummary) Balance() int64 { return s.Received - s.Sent }

func encodeAddrSummary(s AddrSummary) []byte {
	b := make([]byte, 0, 8+8+4+1+2+len(s.Script))
	var tmp [8]byte
	binary.BigEndian.PutUint64(tmp[:], uint64(s.Received))
	b = append(b, tmp[:]...)
	binary.BigEndian.PutUint64(tmp[:], uint64(s.Sent))
	b = append(b, tmp[:]...)
	binary.BigEndian.PutUint32(tmp[:4], s.TxCount)
	b = append(b, tmp[:4]...)
	b = append(b, s.ScriptVersion)
	binary.BigEndian.PutUint16(tmp[:2], uint16(len(s.Script)))
	b = append(b, tmp[:2]...)
	return append(b, s.Script...)
}

func decodeAddrSummary(b []byte) (AddrSummary, error) {
	if len(b) < 23 {
		return AddrSummary{}, fmt.Errorf("store: a corrupted address summary (%d B)", len(b))
	}
	var s AddrSummary
	s.Received = int64(binary.BigEndian.Uint64(b[0:8]))
	s.Sent = int64(binary.BigEndian.Uint64(b[8:16]))
	s.TxCount = binary.BigEndian.Uint32(b[16:20])
	s.ScriptVersion = b[20]
	n := int(binary.BigEndian.Uint16(b[21:23]))
	if len(b) != 23+n {
		return AddrSummary{}, fmt.Errorf("store: the address summary reports a script of %d B, it has %d", n, len(b)-23)
	}
	s.Script = append([]byte(nil), b[23:]...)
	return s, nil
}

func encodeAddrEntry(v int64) []byte {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(v))
	return b[:]
}

// HasAddrIndex says whether the storage keeps an address index.
func (s *Store) HasAddrIndex() bool { return s.indexAddr }

// AddrSummary returns an address's summary. An unknown address has a zero summary, not an error — an
// address nothing ever arrived at is a valid address with a zero balance.
func (s *Store) AddrSummary(sk ScriptKey) (AddrSummary, error) {
	if !s.indexAddr {
		return AddrSummary{}, ErrNoAddrIndex
	}
	b, err := s.get(keyAddrSummary(sk))
	if err == ErrNotFound {
		return AddrSummary{}, nil
	}
	if err != nil {
		return AddrSummary{}, err
	}
	return decodeAddrSummary(b)
}

// AddrHistory returns an address's movements from the newest. `before` is the height to go below
// (0 = from the tip); `limit` is the cap on the number of entries.
//
// It is keyset paging through a prefix scan, not an offset — with an address with a long history an
// offset gets linearly more expensive with every further page.
func (s *Store) AddrHistory(sk ScriptKey, before uint64, limit int) ([]AddrEntry, error) {
	if !s.indexAddr {
		return nil, ErrNoAddrIndex
	}
	if limit <= 0 {
		limit = 50
	}
	lower := keyAddrPrefix(sk)
	upper := keyAddrPrefix(sk)
	upper = append(upper, 0xFF)
	if before > 0 {
		// The upper bound is exclusive, so `before` means "strictly below this height".
		upper = keyAddrEntry(sk, before, [32]byte{}, 0, 0)
	}
	it, err := s.db.NewIter(&pebble.IterOptions{LowerBound: lower, UpperBound: upper})
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	defer it.Close()

	out := make([]AddrEntry, 0, limit)
	for ok := it.Last(); ok && len(out) < limit; ok = it.Prev() {
		k := it.Key()
		if len(k) != 1+32+8+32+1+4 {
			continue
		}
		v := it.Value()
		if len(v) != 8 {
			continue
		}
		var e AddrEntry
		e.Height = binary.BigEndian.Uint64(k[33:41])
		copy(e.TxID[:], k[41:73])
		e.Received = k[73] == dirReceived
		e.N = binary.BigEndian.Uint32(k[74:78])
		e.Value = int64(binary.BigEndian.Uint64(v))
		out = append(out, e)
	}
	return out, it.Error()
}

// addrDelta is the change of one address's summary within one block.
type addrDelta struct {
	received int64
	sent     int64
	txs      map[[32]byte]struct{} // the transactions that touched the address
	version  uint8
	script   []byte
}

// collectAddrDeltas walks a block and produces the list of movements and of summary changes.
//
// The inputs are taken from the undo data, not from the block: the undo data carries the **spent Coin
// including its script**, so the previous output does not have to be looked up. It is the same piece
// of data Bitcoin introduced `getblock` verbosity 3 for.
func collectAddrDeltas(b *chain.Block, undo *chain.BlockUndo, height uint64) (
	[]struct {
		key   []byte
		value []byte
	}, map[ScriptKey]*addrDelta) {

	entries := make([]struct {
		key   []byte
		value []byte
	}, 0, len(undo.Created)+len(undo.Spent))
	deltas := make(map[ScriptKey]*addrDelta)

	touch := func(sk ScriptKey, version uint8, script []byte, txid [32]byte) *addrDelta {
		d := deltas[sk]
		if d == nil {
			d = &addrDelta{txs: make(map[[32]byte]struct{}), version: version, script: script}
			deltas[sk] = d
		}
		d.txs[txid] = struct{}{}
		return d
	}

	for _, tx := range b.Txs {
		txid := tx.TxID()
		for n, out := range tx.Outputs {
			sk := ScriptKeyFor(out.ScriptVersion, out.Script)
			d := touch(sk, out.ScriptVersion, out.Script, txid)
			d.received += out.Value
			entries = append(entries, struct {
				key   []byte
				value []byte
			}{keyAddrEntry(sk, height, txid, dirReceived, uint32(n)), encodeAddrEntry(out.Value)})
		}
	}

	// The spent outputs. The undo data lists them in the order they were spent across the block;
	// an input has to be paired with the transaction that spent it.
	spentAt := make(map[chain.Outpoint]struct {
		txid [32]byte
		n    uint32
	}, len(undo.Spent))
	for _, tx := range b.Txs {
		if tx.IsCoinbase() {
			continue
		}
		txid := tx.TxID()
		for n, in := range tx.Inputs {
			spentAt[chain.Outpoint{TxID: in.PrevTxID, Index: in.PrevIndex}] = struct {
				txid [32]byte
				n    uint32
			}{txid, uint32(n)}
		}
	}
	for _, e := range undo.Spent {
		at, ok := spentAt[e.Outpoint]
		if !ok {
			continue // should not happen; better to leave it out than to write nonsense
		}
		sk := ScriptKeyFor(e.Coin.ScriptVersion, e.Coin.Script)
		d := touch(sk, e.Coin.ScriptVersion, e.Coin.Script, at.txid)
		d.sent += e.Coin.Value
		entries = append(entries, struct {
			key   []byte
			value []byte
		}{keyAddrEntry(sk, height, at.txid, dirSpent, at.n), encodeAddrEntry(e.Coin.Value)})
	}
	return entries, deltas
}

// indexBlockAddrs writes the movements and moves the summaries.
func (s *Store) indexBlockAddrs(batch *pebble.Batch, b *chain.Block, undo *chain.BlockUndo,
	height uint64) error {
	if !s.indexAddr {
		return nil
	}
	entries, deltas := collectAddrDeltas(b, undo, height)
	for _, e := range entries {
		if err := batch.Set(e.key, e.value, nil); err != nil {
			return fmt.Errorf("store: the address index: %w", err)
		}
	}
	return s.applyAddrDeltas(batch, deltas, 1)
}

// unindexBlockAddrs removes a block's movements and moves the summaries back.
func (s *Store) unindexBlockAddrs(batch *pebble.Batch, b *chain.Block, undo *chain.BlockUndo,
	height uint64) error {
	if !s.indexAddr {
		return nil
	}
	entries, deltas := collectAddrDeltas(b, undo, height)
	for _, e := range entries {
		if err := batch.Delete(e.key, nil); err != nil {
			return fmt.Errorf("store: the address index: %w", err)
		}
	}
	return s.applyAddrDeltas(batch, deltas, -1)
}

// applyAddrDeltas reads the summaries, moves them and writes them back. `sign` is +1 when applying a
// block and -1 when unwinding one.
//
// It reads from the database, not from the batch: applying blocks is serial (checkFollowsTip enforces
// that), so no other block can come in between the read and the commit.
func (s *Store) applyAddrDeltas(batch *pebble.Batch, deltas map[ScriptKey]*addrDelta, sign int64) error {
	for sk, d := range deltas {
		cur, err := s.AddrSummary(sk)
		if err != nil {
			return err
		}
		oldBalance := cur.Balance()
		cur.Received += sign * d.received
		cur.Sent += sign * d.sent
		if sign > 0 {
			cur.TxCount += uint32(len(d.txs))
		} else if uint32(len(d.txs)) <= cur.TxCount {
			cur.TxCount -= uint32(len(d.txs))
		} else {
			cur.TxCount = 0
		}
		if cur.Script == nil {
			cur.ScriptVersion, cur.Script = d.version, d.script
		}

		if err := updateRich(batch, sk, oldBalance, cur.Balance()); err != nil {
			return err
		}

		// An address with no history is dropped from the index — otherwise zeros would be left
		// everywhere after an unwind and the rich list would have to skip them.
		if cur.Received == 0 && cur.Sent == 0 && cur.TxCount == 0 {
			if err := batch.Delete(keyAddrSummary(sk), nil); err != nil {
				return fmt.Errorf("store: the address index: %w", err)
			}
			continue
		}
		if err := batch.Set(keyAddrSummary(sk), encodeAddrSummary(cur), nil); err != nil {
			return fmt.Errorf("store: the address index: %w", err)
		}
	}
	return nil
}

// ReindexAddrs builds the address index from scratch.
//
// It needs the **undo data** of every block — without it there is no way to find out what each input
// spent and whose that output was. The storage keeps the undo data for every applied block, so this
// works; a block without undo data (or without a body) has its height skipped, the same as with the
// txindex.
func (s *Store) ReindexAddrs(progress func(height, tip uint64)) error {
	if !s.indexAddr {
		return ErrNoAddrIndex
	}
	tip, err := s.Tip()
	if err != nil {
		return err
	}
	// It commits after EVERY block, not in batches.
	//
	// `applyAddrDeltas` reads the current summary from the database, not from the open batch. If
	// several blocks were batched together, the second block would read the state from before the
	// first and its write would overwrite the first one's contribution — a silent loss that only shows
	// up as a wrong balance. (With ordinary block application this problem does not exist: there the
	// commit happens after every block.)

	for h := uint64(0); h <= tip.Height; h++ {
		id, err := s.BlockIDAt(h)
		if err != nil {
			return fmt.Errorf("store: address reindex: height %d: %w", h, err)
		}
		if !s.HasBody(id) {
			continue
		}
		ub, err := s.get(keyWithID(prefixUndo, id))
		if err != nil {
			continue // a block without undo data is skipped, the same as one without a body
		}
		undo, err := decodeUndo(ub)
		if err != nil {
			return fmt.Errorf("store: address reindex: height %d: %w", h, err)
		}
		blk, err := s.Block(id)
		if err != nil {
			return fmt.Errorf("store: address reindex: height %d: %w", h, err)
		}
		// The summaries move block by block, so the batch has to be committed as we go —
		// applyAddrDeltas reads the current state from the database, not from the open batch.
		batch := s.db.NewBatch()
		if err := s.indexBlockAddrs(batch, blk, undo, h); err != nil {
			batch.Close()
			return err
		}
		if err := batch.Commit(pebble.NoSync); err != nil {
			batch.Close()
			return fmt.Errorf("store: address reindex: %w", err)
		}
		batch.Close()
		if progress != nil && h%1000 == 0 {
			progress(h, tip.Height)
		}
	}
	return s.db.Set(keyAddrIndexed, []byte{1}, pebble.Sync)
}

// AddrIndexReady says whether the address index covers the whole chain.
func (s *Store) AddrIndexReady() bool {
	_, err := s.get(keyAddrIndexed)
	return err == nil
}

// The rich list: `0x09 <inverted balance:8 BE><script hash:32>` -> ∅
//
// The balance is inverted (`math.MaxInt64 - balance`), so an **ascending** scan of the keys returns
// the addresses **descending** by balance. No sorting at runtime, no `ORDER BY` — the top 100 is the
// first hundred keys.
//
// An address is in the list only while its balance is positive. Zero and negative ones do not get in,
// so the shares add up to 100 % — iquidus has the issue "Richlist pie chart sums to 126%", and it
// comes precisely from rows that do not belong there getting into the pie.

func keyRich(balance int64, sk ScriptKey) []byte {
	k := make([]byte, 1+8+32)
	k[0] = prefixRich
	binary.BigEndian.PutUint64(k[1:], uint64(math.MaxInt64-balance))
	copy(k[9:], sk[:])
	return k
}

// RichEntry is one row of the rich list.
type RichEntry struct {
	Key     ScriptKey
	Balance int64
	// ScriptVersion and Script allow an address to be built without another read.
	ScriptVersion uint8
	Script        []byte
}

// RichList returns the addresses ordered descending by balance.
func (s *Store) RichList(limit int) ([]RichEntry, error) {
	if !s.indexAddr {
		return nil, ErrNoAddrIndex
	}
	if limit <= 0 {
		limit = 100
	}
	it, err := s.db.NewIter(&pebble.IterOptions{
		LowerBound: []byte{prefixRich},
		UpperBound: []byte{prefixRich + 1},
	})
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	defer it.Close()

	out := make([]RichEntry, 0, limit)
	for ok := it.First(); ok && len(out) < limit; ok = it.Next() {
		k := it.Key()
		if len(k) != 1+8+32 {
			continue
		}
		var e RichEntry
		e.Balance = int64(math.MaxInt64 - binary.BigEndian.Uint64(k[1:9]))
		copy(e.Key[:], k[9:])
		sum, err := s.AddrSummary(e.Key)
		if err != nil {
			return nil, err
		}
		e.ScriptVersion, e.Script = sum.ScriptVersion, sum.Script
		out = append(out, e)
	}
	return out, it.Error()
}

// updateRich rewrites an address's position in the rich list. The old key has to be deleted,
// otherwise the address would appear in the list twice with two different balances.
func updateRich(batch *pebble.Batch, sk ScriptKey, oldBalance, newBalance int64) error {
	if oldBalance > 0 {
		if err := batch.Delete(keyRich(oldBalance, sk), nil); err != nil {
			return fmt.Errorf("store: rich list: %w", err)
		}
	}
	if newBalance > 0 {
		if err := batch.Set(keyRich(newBalance, sk), nil, nil); err != nil {
			return fmt.Errorf("store: rich list: %w", err)
		}
	}
	return nil
}
