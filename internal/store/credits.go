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

func keyCreditNode(k chain.CreditNodeKey) []byte {
	b := make([]byte, 2+32)
	b[0], b[1] = prefixCreditNode, k.Depth
	copy(b[2:], k.Prefix[:])
	return b
}

func keyCreditAt(prefix byte, at uint64, payout [32]byte) []byte {
	b := make([]byte, 1+8+32)
	b[0] = prefix
	binary.BigEndian.PutUint64(b[1:9], at)
	copy(b[9:], payout[:])
	return b
}

// creditIndexVersion is the version of the indexes below; a directory whose mark carries another
// builds them again. 2: the keys by their last share and the count.
const creditIndexVersion = 2

// creditIndexSpaces are the key spaces the indexes take.
var creditIndexSpaces = []byte{prefixCreditNode, prefixCreditQueue, prefixCreditExpiry, prefixCreditDust, prefixCreditRecent}

// creditIndexKeys are the index entries a credit has: its place in the payout queue, if it reached
// its payout, its expiry, in the dust space or the other, and its last share.
func creditIndexKeys(p [32]byte, c chain.Credit) [][]byte {
	out := [][]byte{keyCreditAt(prefixCreditRecent, ^c.LastShare, p)}
	if from, ok := chain.CreditQueueAt(c); ok {
		out = append(out, keyCreditAt(prefixCreditQueue, from, p))
	}
	space := prefixCreditExpiry
	if chain.CreditIsDust(c) {
		space = prefixCreditDust
	}
	return append(out, keyCreditAt(space, chain.CreditExpiry(c), p))
}

// creditReader is what the credit reads need: the database or a batch that sees its own writes.
type creditReader interface {
	potReader
	creditIterable
}

func readCredit(r creditReader, p [32]byte) (chain.Credit, bool, error) {
	v, closer, err := r.Get(keyCredit(p))
	if errors.Is(err, pebble.ErrNotFound) {
		return chain.Credit{}, false, nil
	}
	if err != nil {
		return chain.Credit{}, false, fmt.Errorf("store: %w", err)
	}
	defer closer.Close()
	c, err := chain.DeserializeCredit(v)
	if err != nil {
		return chain.Credit{}, false, fmt.Errorf("store: the credit of %x is corrupted: %w", p[:6], err)
	}
	return c, true, nil
}

func creditsUnder(r creditReader, prefix [32]byte, depth, max int) ([]chain.CreditEntry, error) {
	lo, hi := chain.CreditPrefixRange(prefix, depth)
	upper := append(keyCredit(hi), 0) // just past hi
	it, err := r.NewIter(&pebble.IterOptions{LowerBound: keyCredit(lo), UpperBound: upper})
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	defer it.Close()
	var out []chain.CreditEntry
	for it.First(); it.Valid() && len(out) < max; it.Next() {
		var p [32]byte
		copy(p[:], it.Key()[1:])
		c, err := chain.DeserializeCredit(it.Value())
		if err != nil {
			return nil, fmt.Errorf("store: the credit of %x is corrupted: %w", p[:6], err)
		}
		out = append(out, chain.CreditEntry{Payout: p, Credit: c})
	}
	return out, it.Error()
}

func creditNode(r creditReader, prefix [32]byte, depth int) ([32]byte, bool, error) {
	v, closer, err := r.Get(keyCreditNode(chain.CreditNodeKey{Depth: uint8(depth), Prefix: prefix}))
	if errors.Is(err, pebble.ErrNotFound) {
		return [32]byte{}, false, nil
	}
	if err != nil {
		return [32]byte{}, false, fmt.Errorf("store: %w", err)
	}
	defer closer.Close()
	if len(v) != 32 {
		return [32]byte{}, false, fmt.Errorf("store: a corrupted credit node (%d B)", len(v))
	}
	var h [32]byte
	copy(h[:], v)
	return h, true, nil
}

// creditQueue reads, in order, at most `max` entries of an index space due at `height` (max < 0:
// all of them), and whether those are all.
func creditQueue(r creditReader, space byte, height uint64, max int) ([]chain.QueueEntry, bool, error) {
	upper := keyCreditAt(space, height, [32]byte{})
	if height == ^uint64(0) {
		upper = []byte{space + 1}
	} else {
		binary.BigEndian.PutUint64(upper[1:9], height+1)
	}
	it, err := r.NewIter(&pebble.IterOptions{LowerBound: []byte{space}, UpperBound: upper})
	if err != nil {
		return nil, false, fmt.Errorf("store: %w", err)
	}
	defer it.Close()
	var out []chain.QueueEntry
	for it.First(); it.Valid(); it.Next() {
		if max >= 0 && len(out) == max {
			return out, false, it.Error()
		}
		k := it.Key()
		if len(k) != 1+8+32 {
			return nil, false, fmt.Errorf("store: a corrupted credit index key (%d B)", len(k))
		}
		e := chain.QueueEntry{At: binary.BigEndian.Uint64(k[1:9])}
		copy(e.Payout[:], k[9:])
		out = append(out, e)
	}
	return out, true, it.Error()
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

// ForEachCredit walks the whole credit state of the applied tip — for tests and the index build,
// never for a block or a request.
func (s *Store) ForEachCredit(fn func([32]byte, chain.Credit) error) error {
	return forEachCredit(s.db, fn)
}

// CreditCount is how many keys hold a credit at the applied tip.
func (s *Store) CreditCount() (int, error) { return readCreditCount(s.db) }

func readCreditCount(r potReader) (int, error) {
	v, closer, err := r.Get(keyCreditCount)
	if errors.Is(err, pebble.ErrNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("store: %w", err)
	}
	defer closer.Close()
	if len(v) != 8 {
		return 0, fmt.Errorf("store: a corrupted credit count (%d B)", len(v))
	}
	return int(binary.LittleEndian.Uint64(v)), nil
}

func encodeCount(n int) []byte { return binary.LittleEndian.AppendUint64(nil, uint64(n)) }

// RecentCredits reads the entries by their last share, the most recent first: at least `max` of
// them, or all there are, and the rest of the last one's block too, so a caller that orders a block's
// entries its own way cuts at the same place. Within a block they come by payout program.
func (s *Store) RecentCredits(max int) ([]chain.CreditEntry, error) {
	it, err := s.db.NewIter(&pebble.IterOptions{LowerBound: []byte{prefixCreditRecent}, UpperBound: []byte{prefixCreditRecent + 1}})
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	defer it.Close()
	var out []chain.CreditEntry
	for it.First(); it.Valid(); it.Next() {
		k := it.Key()
		if len(k) != 1+8+32 {
			return nil, fmt.Errorf("store: a corrupted credit index key (%d B)", len(k))
		}
		last := ^binary.BigEndian.Uint64(k[1:9])
		if len(out) >= max && (len(out) == 0 || out[len(out)-1].Credit.LastShare != last) {
			break
		}
		var p [32]byte
		copy(p[:], k[9:])
		c, ok, err := readCredit(s.db, p)
		if err != nil {
			return nil, err
		}
		if !ok || c.LastShare != last {
			return nil, fmt.Errorf("store: the credit index does not match the credit of %x", p[:6])
		}
		out = append(out, chain.CreditEntry{Payout: p, Credit: c})
	}
	return out, it.Error()
}

// Credit reads one payout program's entry.
func (s *Store) Credit(payout [32]byte) (chain.Credit, bool, error) { return readCredit(s.db, payout) }

func (s *Store) CreditsUnder(prefix [32]byte, depth, max int) ([]chain.CreditEntry, error) {
	return creditsUnder(s.db, prefix, depth, max)
}

func (s *Store) CreditNode(prefix [32]byte, depth int) ([32]byte, bool, error) {
	return creditNode(s.db, prefix, depth)
}

func (s *Store) PayoutQueue(height uint64, max int) ([]chain.QueueEntry, bool, error) {
	return creditQueue(s.db, prefixCreditQueue, height, max)
}

func (s *Store) ExpiredQueue(height uint64, max int) ([]chain.QueueEntry, bool, error) {
	return creditQueue(s.db, prefixCreditExpiry, height, max)
}

func (s *Store) ExpiredDust(height uint64) ([]chain.QueueEntry, error) {
	out, _, err := creditQueue(s.db, prefixCreditDust, height, -1)
	return out, err
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

func (p *pebbleBatch) Credit(payout [32]byte) (chain.Credit, bool, error) {
	return readCredit(p.b, payout)
}

func (p *pebbleBatch) CreditsUnder(prefix [32]byte, depth, max int) ([]chain.CreditEntry, error) {
	return creditsUnder(p.b, prefix, depth, max)
}

func (p *pebbleBatch) CreditNode(prefix [32]byte, depth int) ([32]byte, bool, error) {
	return creditNode(p.b, prefix, depth)
}

func (p *pebbleBatch) PayoutQueue(height uint64, max int) ([]chain.QueueEntry, bool, error) {
	return creditQueue(p.b, prefixCreditQueue, height, max)
}

func (p *pebbleBatch) ExpiredQueue(height uint64, max int) ([]chain.QueueEntry, bool, error) {
	return creditQueue(p.b, prefixCreditExpiry, height, max)
}

func (p *pebbleBatch) ExpiredDust(height uint64) ([]chain.QueueEntry, error) {
	out, _, err := creditQueue(p.b, prefixCreditDust, height, -1)
	return out, err
}

func (p *pebbleBatch) fail(err error) {
	if err != nil && p.err == nil {
		p.err = err
	}
}

// PutCredit writes an entry and moves its index entries with it.
func (p *pebbleBatch) PutCredit(payout [32]byte, c *chain.Credit) {
	old, had, err := readCredit(p.b, payout)
	if err != nil {
		p.fail(err)
		return
	}
	if had {
		for _, k := range creditIndexKeys(payout, old) {
			p.fail(p.b.Delete(k, nil))
		}
	}
	if had != (c != nil) {
		n, err := readCreditCount(p.b)
		p.fail(err)
		if c == nil {
			n--
		} else {
			n++
		}
		p.fail(p.b.Set(keyCreditCount, encodeCount(n), nil))
	}
	if c == nil {
		p.fail(p.b.Delete(keyCredit(payout), nil))
		return
	}
	ser := c.Serialize()
	p.fail(p.b.Set(keyCredit(payout), ser[:], nil))
	for _, k := range creditIndexKeys(payout, *c) {
		p.fail(p.b.Set(k, nil, nil))
	}
}

func (p *pebbleBatch) PutCreditNode(k chain.CreditNodeKey, h *[32]byte) {
	if h == nil {
		p.fail(p.b.Delete(keyCreditNode(k), nil))
	} else {
		p.fail(p.b.Set(keyCreditNode(k), h[:], nil))
	}
}

func (p *pebbleBatch) PutPot(pot int64, root [32]byte) {
	p.fail(p.b.Set(keyPot, encodePot(pot, root), nil))
}

// revertCredits writes the credit state from before a block back into a batch.
func revertCredits(batch *pebble.Batch, undo *chain.BlockUndo) error {
	pb := &pebbleBatch{b: batch}
	if err := chain.RevertCredits(pb, undo); err != nil {
		return err
	}
	return pb.err
}

// ensureCreditIndex builds the credit state's indexes and its tree once, for a directory from
// before them or from an older version of them, and checks the tree against the root the state
// carries.
func (s *Store) ensureCreditIndex() error {
	if v, err := s.get(keyCreditIndexed); err == nil && len(v) == 1 && v[0] == creditIndexVersion {
		return nil
	} else if err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	batch := s.db.NewBatch()
	defer batch.Close()
	for _, space := range creditIndexSpaces {
		if err := batch.DeleteRange([]byte{space}, []byte{space + 1}, nil); err != nil {
			return fmt.Errorf("store: %w", err)
		}
	}
	var entries []chain.CreditEntry
	if err := forEachCredit(s.db, func(p [32]byte, c chain.Credit) error {
		entries = append(entries, chain.CreditEntry{Payout: p, Credit: c})
		for _, k := range creditIndexKeys(p, c) {
			if err := batch.Set(k, nil, nil); err != nil {
				return fmt.Errorf("store: %w", err)
			}
		}
		return nil
	}); err != nil {
		return err
	}
	pot, root, err := readPot(s.db)
	if err != nil {
		return err
	}
	state := make(map[[32]byte]chain.Credit, len(entries))
	for _, e := range entries {
		state[e.Payout] = e.Credit
	}
	if got := chain.CreditsRoot(pot, state); got != root {
		return fmt.Errorf("store: the credit state does not match its root (%x, stored %x)", got[:6], root[:6])
	}
	for k, h := range chain.CreditTreeNodes(entries) {
		if err := batch.Set(keyCreditNode(k), h[:], nil); err != nil {
			return fmt.Errorf("store: %w", err)
		}
	}
	if err := batch.Set(keyCreditCount, encodeCount(len(entries)), nil); err != nil {
		return fmt.Errorf("store: %w", err)
	}
	if err := batch.Set(keyCreditIndexed, []byte{creditIndexVersion}, nil); err != nil {
		return fmt.Errorf("store: %w", err)
	}
	return batch.Commit(pebble.Sync)
}
