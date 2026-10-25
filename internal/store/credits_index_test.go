package store

import (
	"bytes"
	"math/rand/v2"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/cockroachdb/pebble/v2"

	"powmachina/internal/chain"
	"powmachina/internal/mh"
)

func fill(rng *rand.Rand, b []byte) {
	for i := range b {
		b[i] = byte(rng.Uint32())
	}
}

// The credit indexes on disk are always exactly what the entries say: after every block and
// after unwinding each of them, the payout queue, the expiries and the tree's nodes are derived
// from the whole state again and compared, and the step's root with the root of the whole state.
func TestCreditIndexesFollowTheState(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	rng := rand.New(rand.NewPCG(3, 9))
	h := chain.InactiveBlocks + 10_000
	var known [][32]byte
	pick := func() [32]byte {
		if len(known) > 0 && rng.IntN(2) == 0 {
			p := known[rng.IntN(len(known))]
			if rng.IntN(3) == 0 { // a neighbour: a deep tree
				p[31] ^= byte(1 + rng.IntN(255))
				known = append(known, p)
			}
			return p
		}
		var p [32]byte
		fill(rng, p[:])
		known = append(known, p)
		return p
	}
	type saved struct {
		undo *chain.BlockUndo
		dump map[string]string
	}
	var history []saved
	for range 60 {
		commit := &chain.ShareCommit{Payout: pick(), Weight: uint8(rng.IntN(12)), Threshold: uint8(rng.IntN(2))}
		st := &chain.Settlement{}
		for range rng.IntN(30) {
			st.Keys = append(st.Keys, chain.SettleKey{Payout: pick()})
			st.Jobs = append(st.Jobs, chain.SettleJob{Key: uint16(len(st.Keys) - 1), Lane: mh.Lane(rng.IntN(2)), Weight: uint8(rng.IntN(5))})
			st.Shares = append(st.Shares, chain.SettleShare{Job: uint16(len(st.Jobs) - 1)})
		}
		before := dumpCredits(t, s)
		batch := s.db.NewIndexedBatch()
		pb := &pebbleBatch{b: batch}
		undo := &chain.BlockUndo{Height: h, Pot: pb.Pot(), CreditsRoot: pb.CreditsRoot()}
		step, err := chain.CreditStep(h, commit, st, 0, pb)
		if err != nil {
			t.Fatal(err)
		}
		undo.Credits = step.Before
		for p, c := range step.Changed {
			pb.PutCredit(p, c)
		}
		for k, n := range step.Nodes {
			pb.PutCreditNode(k, n)
		}
		pb.PutPot(step.Pot, step.Root)
		if pb.err != nil {
			t.Fatal(pb.err)
		}
		if err := batch.Commit(pebble.Sync); err != nil {
			t.Fatal(err)
		}
		batch.Close()
		checkCreditIndexes(t, s, h)
		history = append(history, saved{undo, before})
		// Sometimes a long gap: keys go inactive, the dust goes back to the pot.
		if rng.IntN(5) == 0 {
			h += chain.InactiveBlocks / 3
		} else {
			h += 1 + uint64(rng.IntN(3000))
		}
	}
	for i := len(history) - 1; i >= 0; i-- {
		batch := s.db.NewIndexedBatch()
		if err := revertCredits(batch, history[i].undo); err != nil {
			t.Fatalf("revert %d: %v", i, err)
		}
		if err := batch.Commit(pebble.Sync); err != nil {
			t.Fatal(err)
		}
		batch.Close()
		if got := dumpCredits(t, s); !reflect.DeepEqual(got, history[i].dump) {
			t.Fatalf("revert %d: the credit key spaces are not what they were", i)
		}
	}
}

// dumpCredits is every key of the credit state and its indexes.
func dumpCredits(t *testing.T, s *Store) map[string]string {
	t.Helper()
	out := map[string]string{}
	it, err := s.db.NewIter(&pebble.IterOptions{LowerBound: []byte{prefixCredit}, UpperBound: []byte{prefixCreditDust + 1}})
	if err != nil {
		t.Fatal(err)
	}
	defer it.Close()
	for it.First(); it.Valid(); it.Next() {
		out[string(it.Key())] = string(it.Value())
	}
	// The pot as read: a store that never had one reads 0 and the empty root, which is what a
	// revert of the first block writes.
	pot, root, err := readPot(s.db)
	if err != nil {
		t.Fatal(err)
	}
	out["pot"] = string(encodePot(pot, root))
	return out
}

func checkCreditIndexes(t *testing.T, s *Store, h uint64) {
	t.Helper()
	var entries []chain.CreditEntry
	state := map[[32]byte]chain.Credit{}
	if err := s.ForEachCredit(func(p [32]byte, c chain.Credit) error {
		entries = append(entries, chain.CreditEntry{Payout: p, Credit: c})
		state[p] = c
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if want := chain.CreditsRoot(s.Pot(), state); s.CreditsRoot() != want {
		t.Fatalf("height %d: the stored root is not the root of the state", h)
	}
	want := map[string]bool{}
	for _, e := range entries {
		for _, k := range creditIndexKeys(e.Payout, e.Credit) {
			want[string(k)] = true
		}
	}
	for k, n := range chain.CreditTreeNodes(entries) {
		want[string(keyCreditNode(k))+string(n[:])] = true
	}
	got := map[string]bool{}
	it, _ := s.db.NewIter(&pebble.IterOptions{LowerBound: []byte{prefixCreditNode}, UpperBound: []byte{prefixCreditDust + 1}})
	for it.First(); it.Valid(); it.Next() {
		k := string(it.Key())
		if it.Key()[0] == prefixCreditNode {
			k += string(it.Value())
		}
		got[k] = true
	}
	it.Close()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("height %d: %d index entries on disk, the state says %d", h, len(got), len(want))
	}
	// The queues read in order and in full.
	for _, at := range []uint64{h, h + chain.InactiveBlocks} {
		q, all, err := s.PayoutQueue(at, -1)
		if err != nil || !all {
			t.Fatal(err)
		}
		var ref []chain.QueueEntry
		for _, e := range entries {
			if from, ok := chain.CreditQueueAt(e.Credit); ok && from <= at {
				ref = append(ref, chain.QueueEntry{At: from, Payout: e.Payout})
			}
		}
		slices.SortFunc(ref, func(a, b chain.QueueEntry) int {
			if a.At != b.At {
				if a.At < b.At {
					return -1
				}
				return 1
			}
			return bytes.Compare(a.Payout[:], b.Payout[:])
		})
		if len(q) != len(ref) || (len(q) > 0 && !reflect.DeepEqual(q, ref)) {
			t.Fatalf("height %d: the payout queue has %d, the state says %d", at, len(q), len(ref))
		}
		if len(ref) > 1 {
			part, all, _ := s.PayoutQueue(at, 1)
			if all || len(part) != 1 || part[0] != ref[0] {
				t.Fatal("a partial read of the queue")
			}
		}
	}
}

// A directory from before the indexes builds them when it opens, and checks them against the root.
func TestCreditIndexBuiltOnOpen(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "db")
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewPCG(5, 5))
	state := map[[32]byte]chain.Credit{}
	batch := s.db.NewBatch()
	for range 500 {
		var p [32]byte
		fill(rng, p[:])
		c := chain.Credit{Amount: int64(rng.IntN(int(chain.POWM))), LastShare: uint64(rng.IntN(100_000)), ReadyAt: uint64(rng.IntN(2) * 50)}
		state[p] = c
		v := c.Serialize()
		batch.Set(keyCredit(p), v[:], nil)
	}
	batch.Set(keyPot, encodePot(77, chain.CreditsRoot(77, state)), nil)
	// What an older node left: the entries and the pot, no indexes, no mark.
	batch.Delete(keyCreditIndexed, nil)
	for _, sp := range []byte{prefixCreditNode, prefixCreditQueue, prefixCreditExpiry, prefixCreditDust} {
		batch.DeleteRange([]byte{sp}, []byte{sp + 1}, nil)
	}
	if err := batch.Commit(pebble.Sync); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	checkCreditIndexes(t, s, 1000)

	// A state that does not match its root is refused, not indexed.
	s.db.Delete(keyCreditIndexed, pebble.Sync)
	s.db.Set(keyPot, encodePot(78, chain.CreditsRoot(77, state)), pebble.Sync)
	if err := s.ensureCreditIndex(); err == nil {
		t.Fatal("a state that does not match its root was indexed")
	}
}
