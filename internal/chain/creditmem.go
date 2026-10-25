package chain

import (
	"bytes"
	"slices"
)

// memCredits is a credit state in memory with the reads a store has (CreditView): for UTXOSet and
// the tests. Where a store reads an index it walks the map, which is fine for the states it holds;
// it is not where a node keeps millions of entries.
type memCredits struct {
	credits map[[32]byte]Credit
	nodes   map[CreditNodeKey][32]byte
	pot     int64
	root    [32]byte
}

func newMemCredits() *memCredits {
	return &memCredits{credits: make(map[[32]byte]Credit), nodes: make(map[CreditNodeKey][32]byte),
		root: CreditsRoot(0, nil)}
}

func (m *memCredits) Pot() int64            { return m.pot }
func (m *memCredits) CreditsRoot() [32]byte { return m.root }

func (m *memCredits) Credit(p [32]byte) (Credit, bool, error) {
	c, ok := m.credits[p]
	return c, ok, nil
}

func (m *memCredits) CreditsUnder(prefix [32]byte, depth, max int) ([]CreditEntry, error) {
	lo, hi := CreditPrefixRange(prefix, depth)
	var out []CreditEntry
	for p, c := range m.credits {
		if bytes.Compare(p[:], lo[:]) >= 0 && bytes.Compare(p[:], hi[:]) <= 0 {
			out = append(out, CreditEntry{p, c})
		}
	}
	slices.SortFunc(out, func(a, b CreditEntry) int { return bytes.Compare(a.Payout[:], b.Payout[:]) })
	if len(out) > max {
		out = out[:max]
	}
	return out, nil
}

func (m *memCredits) CreditNode(prefix [32]byte, depth int) ([32]byte, bool, error) {
	h, ok := m.nodes[CreditNodeKey{uint8(depth), prefix}]
	return h, ok, nil
}

func (m *memCredits) queue(height uint64, max int, at func(Credit) (uint64, bool)) ([]QueueEntry, bool) {
	var out []QueueEntry
	for p, c := range m.credits {
		if a, ok := at(c); ok && a <= height {
			out = append(out, QueueEntry{a, p})
		}
	}
	slices.SortFunc(out, func(a, b QueueEntry) int {
		if queueLess(a, b) {
			return -1
		}
		if queueLess(b, a) {
			return 1
		}
		return 0
	})
	if max >= 0 && len(out) > max {
		return out[:max], false
	}
	return out, true
}

func (m *memCredits) PayoutQueue(height uint64, max int) ([]QueueEntry, bool, error) {
	out, all := m.queue(height, max, CreditQueueAt)
	return out, all, nil
}

func (m *memCredits) ExpiredQueue(height uint64, max int) ([]QueueEntry, bool, error) {
	out, all := m.queue(height, max, func(c Credit) (uint64, bool) { return CreditExpiry(c), !CreditIsDust(c) })
	return out, all, nil
}

func (m *memCredits) ExpiredDust(height uint64) ([]QueueEntry, error) {
	out, _ := m.queue(height, -1, func(c Credit) (uint64, bool) { return CreditExpiry(c), CreditIsDust(c) })
	return out, nil
}

func (m *memCredits) ForEachCredit(fn func([32]byte, Credit) error) error {
	for p, c := range m.credits {
		if err := fn(p, c); err != nil {
			return err
		}
	}
	return nil
}

func (m *memCredits) PutCredit(p [32]byte, c *Credit) {
	if c == nil {
		delete(m.credits, p)
	} else {
		m.credits[p] = *c
	}
}

func (m *memCredits) PutCreditNode(k CreditNodeKey, h *[32]byte) {
	if h == nil {
		delete(m.nodes, k)
	} else {
		m.nodes[k] = *h
	}
}

func (m *memCredits) PutPot(pot int64, root [32]byte) { m.pot, m.root = pot, root }
