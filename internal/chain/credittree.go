package chain

import (
	"bytes"
	"errors"
	"slices"
)

// The credit tree, kept up to date.
//
// CreditsRoot defines the tree over the whole state. A block changes a handful of entries of a
// state that may hold millions, so a store keeps the tree's inner nodes — one for every subtree
// holding two entries or more — and a block recomputes only the paths it changes. The result is
// the same root by construction: every node is CreditNode of its two subtrees exactly as
// creditSubtree computes it, and the tests check the two against each other on random states.

// CreditNodeKey names a subtree: the first Depth bits of Prefix, the rest of Prefix zero.
type CreditNodeKey struct {
	Depth  uint8
	Prefix [32]byte
}

// creditBit is bit `depth` of a program, the most significant first.
func creditBit(p [32]byte, depth int) bool { return p[depth/8]>>(7-depth%8)&1 == 1 }

// withCreditBit sets bit `depth` of a prefix.
func withCreditBit(p [32]byte, depth int) [32]byte {
	p[depth/8] |= 1 << (7 - depth%8)
	return p
}

// CreditPrefixRange is the first and the last program that start with the first `depth` bits of
// `prefix`.
func CreditPrefixRange(prefix [32]byte, depth int) (lo, hi [32]byte) {
	lo, hi = prefix, prefix
	for d := depth; d < 256; d++ {
		lo[d/8] &^= 1 << (7 - d%8)
		hi[d/8] |= 1 << (7 - d%8)
	}
	return lo, hi
}

// creditTreeUpdate is the tree of the state `v` with `changed` applied (nil = deleted): its root
// and the stored nodes that change (nil = the node goes).
func creditTreeUpdate(v CreditView, changed map[[32]byte]*Credit) ([32]byte, map[CreditNodeKey]*[32]byte, error) {
	keys := make([][32]byte, 0, len(changed))
	for p := range changed {
		keys = append(keys, p)
	}
	slices.SortFunc(keys, func(a, b [32]byte) int { return bytes.Compare(a[:], b[:]) })
	t := &treeUpdate{v: v, changed: changed, existed: make(map[[32]byte]bool, len(keys)),
		nodes: make(map[CreditNodeKey]*[32]byte)}
	for _, p := range keys {
		_, ok, err := v.Credit(p)
		if err != nil {
			return [32]byte{}, nil, stateErr(err)
		}
		t.existed[p] = ok
	}
	root, err := t.sub([32]byte{}, 0, keys)
	if err != nil {
		return [32]byte{}, nil, stateErr(err)
	}
	return root, t.nodes, nil
}

type treeUpdate struct {
	v       CreditView
	changed map[[32]byte]*Credit
	existed map[[32]byte]bool // whether a changed program had an entry before the change
	nodes   map[CreditNodeKey]*[32]byte
}

var errCreditTree = errors.New("the stored credit tree does not match the entries")

// sub is the subtree at (prefix, depth) after the change; `ch` are the changed programs in it,
// sorted.
func (t *treeUpdate) sub(prefix [32]byte, depth int, ch [][32]byte) ([32]byte, error) {
	if len(ch) == 0 {
		// Untouched: the stored node, or the subtree's single entry, or nothing.
		if h, ok, err := t.v.CreditNode(prefix, depth); err != nil || ok {
			return h, err
		}
		es, err := t.v.CreditsUnder(prefix, depth, 2)
		if err != nil {
			return [32]byte{}, err
		}
		switch len(es) {
		case 0:
			return [32]byte{}, nil
		case 1:
			return CreditLeaf(es[0].Payout, es[0].Credit), nil
		}
		return [32]byte{}, errCreditTree
	}
	// Mostly the stored node and the changed keys tell how many entries the subtree keeps, without
	// reading the entries: a node means two or more were there, and with nothing deleted they stay.
	existing, kept, deleted := 0, 0, 0
	for _, p := range ch {
		if t.existed[p] {
			existing++
		}
		if t.changed[p] != nil {
			kept++
		} else if t.existed[p] {
			deleted++
		}
	}
	_, hasNode, err := t.v.CreditNode(prefix, depth)
	if err != nil {
		return [32]byte{}, err
	}
	if kept >= 2 || (hasNode && deleted == 0) {
		return t.split(prefix, depth, ch)
	}
	if !hasNode {
		// One entry at most was here. If it was a changed one, that is all; otherwise it may be an
		// untouched one.
		if existing >= 2 {
			return [32]byte{}, errCreditTree
		}
		var after []CreditEntry
		if existing == 0 {
			if after, err = t.v.CreditsUnder(prefix, depth, 1); err != nil {
				return [32]byte{}, err
			}
		}
		for _, p := range ch {
			if c := t.changed[p]; c != nil {
				after = append(after, CreditEntry{Payout: p, Credit: *c})
			}
		}
		switch len(after) {
		case 0:
			return [32]byte{}, nil
		case 1:
			return CreditLeaf(after[0].Payout, after[0].Credit), nil
		}
		return t.split(prefix, depth, ch)
	}
	// Entries go from a subtree that had two or more: read enough of them to tell whether two or
	// more are left once the changed ones are taken out; when fewer are left, these are all there were.
	stored, err := t.v.CreditsUnder(prefix, depth, 2+len(ch))
	if err != nil {
		return [32]byte{}, err
	}
	var after []CreditEntry
	for _, e := range stored {
		if _, ok := t.changed[e.Payout]; !ok {
			after = append(after, e)
		}
	}
	for _, p := range ch {
		if c := t.changed[p]; c != nil {
			after = append(after, CreditEntry{Payout: p, Credit: *c})
		}
	}
	if len(after) >= 2 {
		return t.split(prefix, depth, ch)
	}
	// One entry or none is left: no node here or below. The ones there were go — they stood where
	// two or more of the entries before the change shared a prefix.
	before := make([][32]byte, 0, len(stored))
	for _, e := range stored {
		before = append(before, e.Payout)
	}
	t.drop(prefix, depth, before)
	if len(after) == 0 {
		return [32]byte{}, nil
	}
	return CreditLeaf(after[0].Payout, after[0].Credit), nil
}

// split is a subtree that keeps two entries or more: a node over its two halves.
func (t *treeUpdate) split(prefix [32]byte, depth int, ch [][32]byte) ([32]byte, error) {
	at, _ := slices.BinarySearchFunc(ch, depth, func(k [32]byte, d int) int {
		if !creditBit(k, d) {
			return -1
		}
		return 1
	})
	left, err := t.sub(prefix, depth+1, ch[:at])
	if err != nil {
		return [32]byte{}, err
	}
	right, err := t.sub(withCreditBit(prefix, depth), depth+1, ch[at:])
	if err != nil {
		return [32]byte{}, err
	}
	h := CreditNode(left, right)
	t.nodes[CreditNodeKey{uint8(depth), prefix}] = &h
	return h, nil
}

// drop removes the nodes of a subtree that held the sorted `keys`.
func (t *treeUpdate) drop(prefix [32]byte, depth int, keys [][32]byte) {
	if len(keys) < 2 {
		return
	}
	t.nodes[CreditNodeKey{uint8(depth), prefix}] = nil
	split, _ := slices.BinarySearchFunc(keys, depth, func(k [32]byte, d int) int {
		if !creditBit(k, d) {
			return -1
		}
		return 1
	})
	t.drop(prefix, depth+1, keys[:split])
	t.drop(withCreditBit(prefix, depth), depth+1, keys[split:])
}

// CreditTreeNodes is every stored node of a whole state — for a store that builds them once from
// the entries it already has.
func CreditTreeNodes(state []CreditEntry) map[CreditNodeKey][32]byte {
	slices.SortFunc(state, func(a, b CreditEntry) int { return bytes.Compare(a.Payout[:], b.Payout[:]) })
	keys := make([][32]byte, len(state))
	leaves := make([][32]byte, len(state))
	for i, e := range state {
		keys[i], leaves[i] = e.Payout, CreditLeaf(e.Payout, e.Credit)
	}
	out := make(map[CreditNodeKey][32]byte)
	creditSubtreeEmit(keys, leaves, 0, [32]byte{}, func(k CreditNodeKey, h [32]byte) { out[k] = h })
	return out
}

// RevertCredits puts the credit state from before a block back into a batch: the entries, the
// tree's nodes and the pot. The root it comes to has to be the one the undo remembers.
func RevertCredits(b CreditBatch, undo *BlockUndo) error {
	if undo.Height == 0 {
		return nil // the genesis changed no credits
	}
	tree, nodes, err := creditTreeUpdate(b, undo.Credits)
	if err != nil {
		return err
	}
	if creditsRootFromTree(undo.Pot, tree) != undo.CreditsRoot {
		return stateErr(errors.New("the credit state after the undo does not have the root from before the block"))
	}
	for p, c := range undo.Credits {
		b.PutCredit(p, c)
	}
	for k, h := range nodes {
		b.PutCreditNode(k, h)
	}
	b.PutPot(undo.Pot, undo.CreditsRoot)
	return nil
}
