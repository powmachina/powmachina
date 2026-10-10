package store

import (
	"encoding/binary"
	"fmt"

	"github.com/cockroachdb/pebble/v2"

	"powmachina/internal/chain"
)

// The transaction index: `txid -> (block, position in the block)`.
//
// Without it a transaction **cannot be found by txid**. `prefixCoin` is `<txid><vout>` and holds only
// **unspent** outputs — as soon as an output is spent, the key disappears and the transaction is
// untraceable for the node, although the block's body still contains it. Bitcoin has it the same way
// and that is why it has `-txindex`.
//
// The index is **optional**: a seed node that only verifies and relays does not need it and has no
// reason to pay for it with space. It is turned on when the storage is opened.
//
// It is written **in the same atomic batch as the block** and it is unwound together with it during a
// reorg. That is the whole reason the index is here and not in a separate service over RPC: no
// difference can arise between the chain and the index, not even when the node is killed with `kill -9`.

// TxLocation says where a transaction lies.
type TxLocation struct {
	BlockID [32]byte
	// Index is the transaction's position in the block; 0 is the coinbase.
	Index uint32
}

func keyTxLoc(txid [32]byte) []byte {
	k := make([]byte, 1+32)
	k[0] = prefixTxLoc
	copy(k[1:], txid[:])
	return k
}

func encodeTxLoc(l TxLocation) []byte {
	b := make([]byte, 32+4)
	copy(b, l.BlockID[:])
	binary.BigEndian.PutUint32(b[32:], l.Index)
	return b
}

func decodeTxLoc(b []byte) (TxLocation, error) {
	if len(b) != 36 {
		return TxLocation{}, fmt.Errorf("store: a corrupted txindex record (%d B)", len(b))
	}
	var l TxLocation
	copy(l.BlockID[:], b[:32])
	l.Index = binary.BigEndian.Uint32(b[32:])
	return l, nil
}

// HasTxIndex says whether the storage keeps a transaction index.
func (s *Store) HasTxIndex() bool { return s.indexTx }

// TxLocation finds the block and the position of a transaction. Without the index on it returns
// ErrNoTxIndex — deliberately not "not found", so the caller can tell a missing transaction from a
// missing index.
func (s *Store) TxLocation(txid [32]byte) (TxLocation, error) {
	if !s.indexTx {
		return TxLocation{}, ErrNoTxIndex
	}
	b, err := s.get(keyTxLoc(txid))
	if err != nil {
		return TxLocation{}, err
	}
	return decodeTxLoc(b)
}

// Tx finds a transaction by txid and returns it together with the block it lies in.
func (s *Store) Tx(txid [32]byte) (*chain.Tx, *chain.Block, error) {
	loc, err := s.TxLocation(txid)
	if err != nil {
		return nil, nil, err
	}
	blk, err := s.Block(loc.BlockID)
	if err != nil {
		return nil, nil, err
	}
	if int(loc.Index) >= len(blk.Txs) {
		return nil, nil, fmt.Errorf("store: the txindex points at transaction %d, the block has %d",
			loc.Index, len(blk.Txs))
	}
	return blk.Txs[loc.Index], blk, nil
}

// indexBlockTxs adds the records for all of a block's transactions to the batch.
func (s *Store) indexBlockTxs(batch *pebble.Batch, b *chain.Block, id [32]byte) error {
	if !s.indexTx {
		return nil
	}
	for i, tx := range b.Txs {
		loc := TxLocation{BlockID: id, Index: uint32(i)}
		if err := batch.Set(keyTxLoc(tx.TxID()), encodeTxLoc(loc), nil); err != nil {
			return fmt.Errorf("store: txindex: %w", err)
		}
	}
	return nil
}

// unindexBlockTxs removes a block's records from the batch when it is being unwound.
//
// A record is deleted regardless of whether another block contains the same transaction: on the
// active branch a txid can appear at most once (a duplicate outpoint would fail validation), and if
// the same transaction makes it into a new block, the index is rewritten when that one is applied.
func (s *Store) unindexBlockTxs(batch *pebble.Batch, b *chain.Block) error {
	if !s.indexTx {
		return nil
	}
	for _, tx := range b.Txs {
		if err := batch.Delete(keyTxLoc(tx.TxID()), nil); err != nil {
			return fmt.Errorf("store: txindex: %w", err)
		}
	}
	return nil
}

// ReindexTxs walks the whole chain and builds the index from scratch.
//
// It is called at startup when the index is turned on over a directory that does not have one.
// Without it the index would cover only the blocks added since then and `getrawtransaction` would
// silently return "not found" for older ones — which is worse than an error, because it looks like a
// valid answer.
func (s *Store) ReindexTxs(progress func(height, tip uint64)) error {
	if !s.indexTx {
		return ErrNoTxIndex
	}
	tip, err := s.Tip()
	if err != nil {
		return err
	}
	const chunk = 500
	var skipped int
	batch := s.db.NewBatch()
	defer batch.Close()

	for h := uint64(0); h <= tip.Height; h++ {
		id, err := s.BlockIDAt(h)
		if err != nil {
			return fmt.Errorf("store: reindex: height %d: %w", h, err)
		}
		// A header without a body is a legitimate state: the genesis is created as a header only
		// (InitGenesis) and while downloading, bodies arrive out of order. What we do not have cannot
		// be handed out anyway — `Tx` would fail on a missing body just the same.
		if !s.HasBody(id) {
			skipped++
			continue
		}
		blk, err := s.Block(id)
		if err != nil {
			return fmt.Errorf("store: reindex of height %d: %w", h, err)
		}
		if err := s.indexBlockTxs(batch, blk, id); err != nil {
			return err
		}
		if batch.Count() >= chunk {
			if err := batch.Commit(pebble.NoSync); err != nil {
				return fmt.Errorf("store: reindex: %w", err)
			}
			batch.Close()
			batch = s.db.NewBatch()
		}
		if progress != nil && h%1000 == 0 {
			progress(h, tip.Height)
		}
	}
	if err := batch.Commit(pebble.Sync); err != nil {
		return fmt.Errorf("store: reindex: %w", err)
	}
	if progress != nil && skipped > 0 {
		progress(tip.Height, tip.Height) // the caller can add that something was missing
	}
	return s.db.Set(keyTxIndexed, []byte{1}, pebble.Sync)
}

// TxIndexReady says whether the index covers the whole chain (that is, whether a reindex has run).
func (s *Store) TxIndexReady() bool {
	_, err := s.get(keyTxIndexed)
	return err == nil
}
