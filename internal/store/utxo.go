package store

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/cockroachdb/pebble/v2"
	"powmachina/internal/chain"
)

// ---------------------------------------------------------------- coin serialization

// A Coin is stored compactly and with a **fixed field order**. No universal encoder is used here on
// purpose: the on-disk format is something that has to be readable in five years too, and reflection
// would turn it into a moving target.
func encodeCoin(c chain.Coin) []byte {
	b := make([]byte, 0, 8+1+8+1+4+len(c.Script))
	var num [8]byte
	binary.LittleEndian.PutUint64(num[:], uint64(c.Value))
	b = append(b, num[:]...)
	b = append(b, c.ScriptVersion)
	binary.LittleEndian.PutUint64(num[:], c.Height)
	b = append(b, num[:]...)
	if c.Coinbase {
		b = append(b, 1)
	} else {
		b = append(b, 0)
	}
	binary.LittleEndian.PutUint32(num[:4], uint32(len(c.Script)))
	b = append(b, num[:4]...)
	return append(b, c.Script...)
}

func decodeCoin(b []byte) (chain.Coin, error) {
	var c chain.Coin
	if len(b) < 22 {
		return c, fmt.Errorf("store: a corrupted coin (%d B)", len(b))
	}
	c.Value = int64(binary.LittleEndian.Uint64(b[0:8]))
	c.ScriptVersion = b[8]
	c.Height = binary.LittleEndian.Uint64(b[9:17])
	c.Coinbase = b[17] == 1
	n := binary.LittleEndian.Uint32(b[18:22])
	if uint64(len(b)) != 22+uint64(n) {
		return c, fmt.Errorf("store: the coin has a bad script length (%d vs %d)", len(b)-22, n)
	}
	c.Script = append([]byte(nil), b[22:]...)
	return c, nil
}

// ---------------------------------------------------------------- undo

// The undo data is the only thing that makes rolling a block back during a reorg possible. It is
// stored in the same atomic batch as the apply — otherwise a state "the block is applied, it cannot
// be rolled back" would exist.
func encodeUndo(u *chain.BlockUndo) []byte {
	var buf bytes.Buffer
	var num [8]byte
	binary.LittleEndian.PutUint64(num[:], u.Height)
	buf.Write(num[:])

	binary.LittleEndian.PutUint32(num[:4], uint32(len(u.Spent)))
	buf.Write(num[:4])
	for _, e := range u.Spent {
		buf.Write(e.Outpoint.TxID[:])
		binary.LittleEndian.PutUint32(num[:4], e.Outpoint.Index)
		buf.Write(num[:4])
		c := encodeCoin(e.Coin)
		binary.LittleEndian.PutUint32(num[:4], uint32(len(c)))
		buf.Write(num[:4])
		buf.Write(c)
	}

	binary.LittleEndian.PutUint32(num[:4], uint32(len(u.Created)))
	buf.Write(num[:4])
	for _, op := range u.Created {
		buf.Write(op.TxID[:])
		binary.LittleEndian.PutUint32(num[:4], op.Index)
		buf.Write(num[:4])
	}

	// The credit state before the block: the pot, its root, and every entry the block changed
	// (a flag 0 = the block created it).
	binary.LittleEndian.PutUint64(num[:], uint64(u.Pot))
	buf.Write(num[:])
	buf.Write(u.CreditsRoot[:])
	binary.LittleEndian.PutUint32(num[:4], uint32(len(u.Credits)))
	buf.Write(num[:4])
	for p, c := range u.Credits {
		buf.Write(p[:])
		if c == nil {
			buf.WriteByte(0)
			continue
		}
		buf.WriteByte(1)
		ser := c.Serialize()
		buf.Write(ser[:])
	}
	return buf.Bytes()
}

func decodeUndo(b []byte) (*chain.BlockUndo, error) {
	r := bytes.NewReader(b)
	readU32 := func() (uint32, error) {
		var n [4]byte
		if _, err := r.Read(n[:]); err != nil {
			return 0, err
		}
		return binary.LittleEndian.Uint32(n[:]), nil
	}
	var num [8]byte
	if _, err := r.Read(num[:]); err != nil {
		return nil, fmt.Errorf("store: corrupted undo data: %w", err)
	}
	u := &chain.BlockUndo{Height: binary.LittleEndian.Uint64(num[:])}

	n, err := readU32()
	if err != nil {
		return nil, fmt.Errorf("store: corrupted undo data: %w", err)
	}
	for i := uint32(0); i < n; i++ {
		var e chain.UndoEntry
		if _, err := r.Read(e.Outpoint.TxID[:]); err != nil {
			return nil, fmt.Errorf("store: corrupted undo data: %w", err)
		}
		if e.Outpoint.Index, err = readU32(); err != nil {
			return nil, fmt.Errorf("store: corrupted undo data: %w", err)
		}
		cl, err := readU32()
		if err != nil {
			return nil, fmt.Errorf("store: corrupted undo data: %w", err)
		}
		cb := make([]byte, cl)
		if _, err := r.Read(cb); err != nil {
			return nil, fmt.Errorf("store: corrupted undo data: %w", err)
		}
		if e.Coin, err = decodeCoin(cb); err != nil {
			return nil, err
		}
		u.Spent = append(u.Spent, e)
	}

	n, err = readU32()
	if err != nil {
		return nil, fmt.Errorf("store: corrupted undo data: %w", err)
	}
	for i := uint32(0); i < n; i++ {
		var op chain.Outpoint
		if _, err := r.Read(op.TxID[:]); err != nil {
			return nil, fmt.Errorf("store: corrupted undo data: %w", err)
		}
		if op.Index, err = readU32(); err != nil {
			return nil, fmt.Errorf("store: corrupted undo data: %w", err)
		}
		u.Created = append(u.Created, op)
	}

	if _, err := r.Read(num[:]); err != nil {
		return nil, fmt.Errorf("store: corrupted undo data: %w", err)
	}
	u.Pot = int64(binary.LittleEndian.Uint64(num[:]))
	if _, err := r.Read(u.CreditsRoot[:]); err != nil {
		return nil, fmt.Errorf("store: corrupted undo data: %w", err)
	}
	if n, err = readU32(); err != nil {
		return nil, fmt.Errorf("store: corrupted undo data: %w", err)
	}
	if n > 0 {
		u.Credits = make(map[[32]byte]*chain.Credit, n)
	}
	for i := uint32(0); i < n; i++ {
		var p [32]byte
		if _, err := r.Read(p[:]); err != nil {
			return nil, fmt.Errorf("store: corrupted undo data: %w", err)
		}
		flag, err := r.ReadByte()
		if err != nil || flag > 1 {
			return nil, fmt.Errorf("store: corrupted undo data: credit flag %d (%v)", flag, err)
		}
		if flag == 0 {
			u.Credits[p] = nil
			continue
		}
		raw := make([]byte, chain.CreditBytes)
		if _, err := io.ReadFull(r, raw); err != nil {
			return nil, fmt.Errorf("store: corrupted undo data: %w", err)
		}
		c, err := chain.DeserializeCredit(raw)
		if err != nil {
			return nil, err
		}
		u.Credits[p] = &c
	}
	if r.Len() != 0 {
		return nil, fmt.Errorf("store: corrupted undo data: %d trailing bytes", r.Len())
	}
	return u, nil
}

// ---------------------------------------------------------------- UTXO

func (s *Store) Coin(op chain.Outpoint) (chain.Coin, bool) {
	c, ok, _ := s.LookupCoin(op)
	return c, ok
}

// LookupCoin is Coin that tells "there is no such coin" from "the coin could not be read". The
// difference decides a block's fate: a block that spends a coin nobody has is invalid for ever, a
// block the disk failed on is not.
func (s *Store) LookupCoin(op chain.Outpoint) (chain.Coin, bool, error) {
	v, err := s.get(keyCoin(op.TxID, op.Index))
	if errors.Is(err, ErrNotFound) {
		return chain.Coin{}, false, nil
	}
	if err != nil {
		return chain.Coin{}, false, err
	}
	c, err := decodeCoin(v)
	if err != nil {
		return chain.Coin{}, false, fmt.Errorf("store: the coin %x:%d is corrupted: %w", op.TxID[:6], op.Index, err)
	}
	return c, true, nil
}

// pebbleBatch is a chain.CoinBatch over an indexed Pebble batch. It has to be indexed because a
// read must see the writes from the same batch — a transaction may spend an output of a previous one
// in the same block.
type pebbleBatch struct {
	b   *pebble.Batch
	err error
}

func (p *pebbleBatch) Get(op chain.Outpoint) (chain.Coin, bool) {
	v, closer, err := p.b.Get(keyCoin(op.TxID, op.Index))
	if err != nil {
		return chain.Coin{}, false // both ErrNotFound and a real error: the coin is not usable
	}
	c, derr := decodeCoin(v)
	closer.Close()
	if derr != nil {
		p.err = derr
		return chain.Coin{}, false
	}
	return c, true
}

func (p *pebbleBatch) Add(op chain.Outpoint, c chain.Coin) {
	if err := p.b.Set(keyCoin(op.TxID, op.Index), encodeCoin(c), nil); err != nil && p.err == nil {
		p.err = err
	}
}

func (p *pebbleBatch) Spend(op chain.Outpoint) {
	if err := p.b.Delete(keyCoin(op.TxID, op.Index), nil); err != nil && p.err == nil {
		p.err = err
	}
}

// Get makes Store a chain.CoinView directly, so validation can verify against disk without the UTXO
// set having to fit into memory.
func (s *Store) Get(op chain.Outpoint) (chain.Coin, bool) {
	return s.Coin(op)
}

// ApplyBlockDirect applies a block **straight to disk** — it computes the undo data itself, through
// the same `chain.ApplyBlockTo` the in-memory set uses. This is the path a node should take: the UTXO
// set does not have to fit into RAM.
func (s *Store) ApplyBlockDirect(b *chain.Block) (*chain.BlockUndo, error) {
	id := b.Header.ID()
	height := uint64(b.Header.Height)
	if err := s.checkFollowsTip(b, height); err != nil {
		return nil, err
	}

	batch := s.db.NewIndexedBatch()
	defer batch.Close()
	pb := &pebbleBatch{b: batch}

	undo, err := chain.ApplyBlockTo(b, pb)
	if err != nil {
		return nil, err // the batch is discarded, nothing on disk changed
	}
	if pb.err != nil {
		return nil, fmt.Errorf("store: %w", pb.err)
	}
	if err := s.finishApply(batch, b, id, height, undo); err != nil {
		return nil, err
	}
	return undo, nil
}

func (s *Store) checkFollowsTip(b *chain.Block, height uint64) error {
	tip, err := s.Tip()
	switch {
	case errors.Is(err, ErrNotFound):
		if height != 0 {
			return fmt.Errorf("%w: the first applied block has to be the genesis, it is %d", ErrTipMismatch, height)
		}
	case err != nil:
		return err
	default:
		if b.Header.PrevBlock != tip.ID {
			return fmt.Errorf("%w: prev %x, tip %x", ErrTipMismatch, b.Header.PrevBlock[:6], tip.ID[:6])
		}
	}
	return nil
}

// advanceHeaderTip moves the verified-headers mark if an applied block has outgrown it.
//
// The header of an applied block is verified by definition, so headerTip must never stay below the
// tip. PutHeader only moves it while headers are being downloaded; a node that mines blocks or
// receives them whole (PutBlock + ApplyBlockDirect) never goes through PutHeader at all. Without
// this call the mark stayed at the genesis and Restore reported height 0 after a restart — the node
// threw away the whole chain and started syncing from scratch, although it had everything on disk.
func (s *Store) advanceHeaderTip(batch *pebble.Batch, id [32]byte, height uint64) error {
	if cur, err := s.HeaderTip(); err == nil && cur.Height >= height {
		return nil
	} else if err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	if err := batch.Set(keyHeaderTip, encodeTip(Tip{ID: id, Height: height}), nil); err != nil {
		return fmt.Errorf("store: %w", err)
	}
	return nil
}

// finishApply adds the undo data, the height index and the tip mark to the batch and commits it.
// This is where consistency after a crash is decided: either everything is written, or nothing.
func (s *Store) finishApply(batch *pebble.Batch, b *chain.Block, id [32]byte, height uint64, undo *chain.BlockUndo) error {
	if err := batch.Set(keyWithID(prefixUndo, id), encodeUndo(undo), nil); err != nil {
		return fmt.Errorf("store: %w", err)
	}
	// The indexes go into the same batch — no difference may arise between the chain and an index.
	if err := s.indexBlockTxs(batch, b, id); err != nil {
		return err
	}
	if err := s.indexBlockAddrs(batch, b, undo, height); err != nil {
		return err
	}
	if err := batch.Set(keyHeight(height), id[:], nil); err != nil {
		return fmt.Errorf("store: %w", err)
	}
	if err := batch.Set(keyTip, encodeTip(Tip{ID: id, Height: height}), nil); err != nil {
		return fmt.Errorf("store: %w", err)
	}
	if err := s.advanceHeaderTip(batch, id, height); err != nil {
		return err
	}
	return batch.Commit(pebble.Sync)
}

// ApplyBlock writes a block's consequences **in one atomic batch**: the spent coins are gone, the
// new ones added, the credits moved, the undo data stored, the height index filled in and the tip
// mark moved.
//
// Either everything happens, or nothing. That is the whole reason Pebble is here and not our own
// code — after `kill -9` in the middle of this function, a state where the UTXO set does not match
// the tip cannot arise.
//
// `undo` is what `chain.UTXOSet.Apply` returned for the same block. The store recomputes it through
// chain.ApplyBlockTo: the credits a block writes are not in an undo, which keeps what was before.
func (s *Store) ApplyBlock(b *chain.Block, undo *chain.BlockUndo) error {
	_, err := s.ApplyBlockDirect(b)
	return err
}

// RevertTip rolls back the block at the tip — for a reorg. Again one atomic batch.
func (s *Store) RevertTip() error {
	tip, err := s.Tip()
	if err != nil {
		return err
	}
	ub, err := s.get(keyWithID(prefixUndo, tip.ID))
	if err != nil {
		return fmt.Errorf("store: the undo data for %x is missing: %w", tip.ID[:6], err)
	}
	undo, err := decodeUndo(ub)
	if err != nil {
		return err
	}
	hdr, err := s.Header(tip.ID)
	if err != nil {
		return err
	}

	batch := s.db.NewIndexedBatch()
	defer batch.Close()

	// The spent coins first, the created ones after — the order chain.UTXOSet.Undo explains: an
	// output created and spent in the same block is in both lists and must end up deleted. The
	// batch applies its operations in order. It also unwinds undo records written before the fix.
	for _, e := range undo.Spent {
		if err := batch.Set(keyCoin(e.Outpoint.TxID, e.Outpoint.Index), encodeCoin(e.Coin), nil); err != nil {
			return fmt.Errorf("store: %w", err)
		}
	}
	for _, op := range undo.Created {
		if err := batch.Delete(keyCoin(op.TxID, op.Index), nil); err != nil {
			return fmt.Errorf("store: %w", err)
		}
	}
	if err := revertCredits(batch, undo); err != nil {
		return fmt.Errorf("store: %w", err)
	}
	// The height index is **not deleted** here: it is the manifest of the active branch of headers,
	// not a record of what is applied. Unwinding the UTXO set is only one step down, the headers stay.
	// Deleting the index here would make a reorg also delete the entries the new branch had just
	// written into it, and `ApplyStored` would have nothing to continue from — the node would stay
	// below the fork forever.
	if err := batch.Delete(keyWithID(prefixUndo, tip.ID), nil); err != nil {
		return fmt.Errorf("store: %w", err)
	}
	// The index is unwound in the same batch as the block. The block's body stays in storage (it may
	// come back onto the active branch), but looking a transaction up on an unwound branch would mean
	// claiming it is in the chain.
	if s.indexTx || s.indexAddr {
		blk, err := s.Block(tip.ID)
		if err != nil {
			return fmt.Errorf("store: unwinding %x needs the block body: %w", tip.ID[:6], err)
		}
		if err := s.unindexBlockTxs(batch, blk); err != nil {
			return err
		}
		if err := s.unindexBlockAddrs(batch, blk, undo, tip.Height); err != nil {
			return err
		}
	}
	if tip.Height == 0 {
		if err := batch.Delete(keyTip, nil); err != nil {
			return fmt.Errorf("store: %w", err)
		}
	} else {
		if err := batch.Set(keyTip, encodeTip(Tip{ID: hdr.PrevBlock, Height: tip.Height - 1}), nil); err != nil {
			return fmt.Errorf("store: %w", err)
		}
	}
	return batch.Commit(pebble.Sync)
}

// coinFromBlock finds the output an outpoint points at. The undo data carries only outpoints, not
// values — the values are in the block being applied.
func coinFromBlock(b *chain.Block, op chain.Outpoint, height uint64) (chain.Coin, bool) {
	for i, tx := range b.Txs {
		if tx.TxID() != op.TxID {
			continue
		}
		if int(op.Index) >= len(tx.Outputs) {
			return chain.Coin{}, false
		}
		o := tx.Outputs[op.Index]
		return chain.Coin{
			Value:         o.Value,
			ScriptVersion: o.ScriptVersion,
			Script:        append([]byte(nil), o.Script...),
			Height:        height,
			Coinbase:      i == 0,
		}, true
	}
	return chain.Coin{}, false
}

// ForEachCoin walks the whole UTXO set. It serves consistency checks and snapshots — not ordinary
// operation.
func (s *Store) ForEachCoin(fn func(chain.Outpoint, chain.Coin) error) error {
	lo := []byte{prefixCoin}
	hi := []byte{prefixCoin + 1}
	it, err := s.db.NewIter(&pebble.IterOptions{LowerBound: lo, UpperBound: hi})
	if err != nil {
		return fmt.Errorf("store: %w", err)
	}
	defer it.Close()
	for it.First(); it.Valid(); it.Next() {
		k := it.Key()
		if len(k) != 1+32+4 {
			return fmt.Errorf("store: a corrupted coin key (%d B)", len(k))
		}
		var op chain.Outpoint
		copy(op.TxID[:], k[1:33])
		op.Index = binary.BigEndian.Uint32(k[33:])
		c, err := decodeCoin(it.Value())
		if err != nil {
			return err
		}
		if err := fn(op, c); err != nil {
			return err
		}
	}
	return it.Error()
}

// CoinCount counts the coins in the set.
func (s *Store) CoinCount() (int, error) {
	n := 0
	err := s.ForEachCoin(func(chain.Outpoint, chain.Coin) error { n++; return nil })
	return n, err
}

// AddCoin inserts a coin outside of applying a block. It is **for tests only** — in production the
// UTXO set changes exclusively through ApplyBlockDirect, so an undo path exists and the tip mark matches.
func (s *Store) AddCoin(op chain.Outpoint, c chain.Coin) error {
	return s.db.Set(keyCoin(op.TxID, op.Index), encodeCoin(c), pebble.Sync)
}

// RemoveCoin deletes a coin outside of applying a block. **For tests only**, the same as AddCoin:
// the tests that verify what happens when a block has spent an output in the meantime need it.
func (s *Store) RemoveCoin(op chain.Outpoint) error {
	return s.db.Delete(keyCoin(op.TxID, op.Index), pebble.Sync)
}
