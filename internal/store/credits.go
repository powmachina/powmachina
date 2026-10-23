package store

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/cockroachdb/pebble/v2"
	"powmachina/internal/chain"
)

// The credit state (chain/credits.go): one entry per payout program, and the pot with the root of
// the state it belongs to. Both change only through ApplyBlockDirect and RevertTip, in the same
// atomic batch as the coins.

// keyPot holds pot (8 B LE) ‖ the credit root (32 B).
var keyPot = []byte{prefixMeta, 'p', 'o', 't'}

func keyCredit(payout [32]byte) []byte { return keyWithID(prefixCredit, payout) }

func encodePot(pot int64, root [32]byte) []byte {
	b := make([]byte, 40)
	binary.LittleEndian.PutUint64(b[0:8], uint64(pot))
	copy(b[8:], root[:])
	return b
}

// emptyCreditsRoot is the root of the state before the first block settled anything.
var emptyCreditsRoot = chain.CreditsRoot(0, nil)

type potReader interface {
	Get(key []byte) ([]byte, io.Closer, error)
}

func readPot(r potReader) (int64, [32]byte, error) {
	v, closer, err := r.Get(keyPot)
	if errors.Is(err, pebble.ErrNotFound) {
		return 0, emptyCreditsRoot, nil
	}
	if err != nil {
		return 0, [32]byte{}, fmt.Errorf("store: %w", err)
	}
	defer closer.Close()
	if len(v) != 40 {
		return 0, [32]byte{}, fmt.Errorf("store: a corrupted pot (%d B)", len(v))
	}
	var root [32]byte
	copy(root[:], v[8:])
	return int64(binary.LittleEndian.Uint64(v[0:8])), root, nil
}

type creditIterable interface {
	NewIter(*pebble.IterOptions) (*pebble.Iterator, error)
}

func forEachCredit(r creditIterable, fn func([32]byte, chain.Credit) error) error {
	it, err := r.NewIter(&pebble.IterOptions{LowerBound: []byte{prefixCredit}, UpperBound: []byte{prefixCredit + 1}})
	if err != nil {
		return fmt.Errorf("store: %w", err)
	}
	defer it.Close()
	for it.First(); it.Valid(); it.Next() {
		k := it.Key()
		if len(k) != 1+32 {
			return fmt.Errorf("store: a corrupted credit key (%d B)", len(k))
		}
		var p [32]byte
		copy(p[:], k[1:])
		c, err := chain.DeserializeCredit(it.Value())
		if err != nil {
			return fmt.Errorf("store: the credit of %x is corrupted: %w", p[:6], err)
		}
		if err := fn(p, c); err != nil {
			return err
		}
	}
	return it.Error()
}

// Pot is the pot of the applied tip's credit state. A read error answers 0; CreditsRoot then
// answers a root no block carries, so a block is refused for it rather than accepted.
func (s *Store) Pot() int64 {
	pot, _, _ := readPot(s.db)
	return pot
}

// CreditsRoot is the root of the applied tip's credit state.
func (s *Store) CreditsRoot() [32]byte {
	_, root, err := readPot(s.db)
	if err != nil {
		return [32]byte{}
	}
	return root
}

// ForEachCredit walks the credit state of the applied tip.
func (s *Store) ForEachCredit(fn func([32]byte, chain.Credit) error) error {
	return forEachCredit(s.db, fn)
}

// Credit reads one payout program's entry.
func (s *Store) Credit(payout [32]byte) (chain.Credit, bool, error) {
	v, err := s.get(keyCredit(payout))
	if errors.Is(err, ErrNotFound) {
		return chain.Credit{}, false, nil
	}
	if err != nil {
		return chain.Credit{}, false, err
	}
	c, err := chain.DeserializeCredit(v)
	return c, err == nil, err
}

func (p *pebbleBatch) Pot() int64 {
	pot, _, err := readPot(p.b)
	if err != nil && p.err == nil {
		p.err = err
	}
	return pot
}

func (p *pebbleBatch) CreditsRoot() [32]byte {
	_, root, err := readPot(p.b)
	if err != nil && p.err == nil {
		p.err = err
	}
	return root
}

func (p *pebbleBatch) ForEachCredit(fn func([32]byte, chain.Credit) error) error {
	return forEachCredit(p.b, fn)
}

func (p *pebbleBatch) PutCredit(payout [32]byte, c *chain.Credit) {
	var err error
	if c == nil {
		err = p.b.Delete(keyCredit(payout), nil)
	} else {
		ser := c.Serialize()
		err = p.b.Set(keyCredit(payout), ser[:], nil)
	}
	if err != nil && p.err == nil {
		p.err = err
	}
}

func (p *pebbleBatch) PutPot(pot int64, root [32]byte) {
	if err := p.b.Set(keyPot, encodePot(pot, root), nil); err != nil && p.err == nil {
		p.err = err
	}
}

// revertCredits writes the credit state from before a block back into a batch.
func revertCredits(batch *pebble.Batch, undo *chain.BlockUndo) error {
	if undo.Height == 0 {
		return nil // the genesis changed no credits
	}
	pb := &pebbleBatch{b: batch}
	for p, c := range undo.Credits {
		pb.PutCredit(p, c)
	}
	pb.PutPot(undo.Pot, undo.CreditsRoot)
	return pb.err
}
