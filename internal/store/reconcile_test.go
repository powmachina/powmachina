package store

import (
	"testing"

	"powmachina/internal/chain"
)

// blkOn builds a sealed block on top of prev at a height, tagged so that two branches at the same
// height get different ids. Nothing is validated — the storage is being tested, not consensus.
func blkOn(prev [32]byte, height uint64, tag byte) *chain.Block {
	params := chain.RegTest
	var extra [32]byte
	extra[0] = byte(height)
	extra[31] = tag
	cb := chain.BuildCoinbase(height, nil, extra, params)
	key := make([]byte, 32)
	key[0] = tag
	key[1] = byte(height)
	cb.Outputs = append(cb.Outputs, chain.TxOut{Value: 1, Script: key})
	b := &chain.Block{
		Header: &chain.Header{
			Version: 1, PrevBlock: prev, Bits: chain.RegtestTargetBits,
			Height: uint32(height), Time: uint32(1_700_000_000 + height*120), Lane: 1,
		},
		Txs: []*chain.Tx{cb},
	}
	b.Seal()
	return b
}

// applyChain stores and applies a linear run of blocks (base + one branch) to the UTXO set, exactly
// as the node does when it extends the tip. After it, the height index, the header tip and the UTXO
// tip all describe this branch.
func applyChain(t *testing.T, s *Store, blocks []*chain.Block) {
	t.Helper()
	utxo := chain.NewUTXOSet()
	for _, b := range blocks {
		if err := s.PutBlock(b); err != nil {
			t.Fatalf("put %x: %v", b.Header.ID(), err)
		}
		undo, err := utxo.Apply(b)
		if err != nil {
			t.Fatalf("apply %x: %v", b.Header.ID(), err)
		}
		if err := s.ApplyBlock(b, undo); err != nil {
			t.Fatalf("store apply %x: %v", b.Header.ID(), err)
		}
	}
}

// ReconcileUTXOToIndex repairs the one durable state a reorg crash can leave: the height index was
// rewritten onto the new branch (SetActiveBranch), but the UTXO set was not yet rolled back to the
// fork. The UTXO tip then sits on a block that is no longer on the active branch. Both variants —
// the new branch taller than the old, and shorter-but-heavier (so the index above its tip is gone)
// — have to come back onto the active branch and stop at the fork.
func TestReconcileUTXOToIndexAfterReorgCrash(t *testing.T) {
	// base: heights 0..2 shared by both branches; branch A extends it, gets applied to the UTXO set.
	base := []*chain.Block{blkOn([32]byte{}, 0, 0xba)}
	base = append(base, blkOn(base[0].Header.ID(), 1, 0xba))
	base = append(base, blkOn(base[1].Header.ID(), 2, 0xba))
	fork := base[2].Header.ID()

	branchA := []*chain.Block{blkOn(fork, 3, 0xaa)}
	branchA = append(branchA, blkOn(branchA[0].Header.ID(), 4, 0xaa))

	cases := []struct {
		name   string
		branchB func() []*chain.Block // the new active branch, by its SetActiveBranch entries
	}{
		{
			name: "taller",
			branchB: func() []*chain.Block {
				b := []*chain.Block{blkOn(fork, 3, 0xbb)}
				b = append(b, blkOn(b[0].Header.ID(), 4, 0xbb))
				b = append(b, blkOn(b[1].Header.ID(), 5, 0xbb))
				return b
			},
		},
		{
			name: "shorter-but-heavier",
			branchB: func() []*chain.Block {
				return []*chain.Block{blkOn(fork, 3, 0xcc)}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, err := OpenMem()
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()

			applyChain(t, s, append(append([]*chain.Block{}, base...), branchA...))

			if tip, err := s.Tip(); err != nil || tip.Height != 4 || tip.ID != branchA[1].Header.ID() {
				t.Fatalf("setup: the UTXO tip is not branch A's: %+v (%v)", tip, err)
			}

			// The committed first half of a reorg: the index names branch B, the UTXO set is still
			// on branch A. A crash here is the state ReconcileUTXOToIndex has to survive.
			branchB := tc.branchB()
			var entries []BranchEntry
			for _, b := range branchB {
				if err := s.PutBlock(b); err != nil {
					t.Fatal(err)
				}
				entries = append(entries, BranchEntry{Height: uint64(b.Header.Height), ID: b.Header.ID()})
			}
			if err := s.SetActiveBranch(entries); err != nil {
				t.Fatal(err)
			}

			// Precondition: the UTXO tip is off the active branch.
			if tip, err := s.Tip(); err != nil {
				t.Fatal(err)
			} else if id, e := s.BlockIDAt(tip.Height); e == nil && id == tip.ID {
				t.Fatalf("setup is wrong: the UTXO tip %+v is already on the active branch", tip)
			}

			if err := s.ReconcileUTXOToIndex(); err != nil {
				t.Fatalf("reconcile: %v", err)
			}

			// After it, the UTXO tip is on the active branch and has stopped at the fork.
			tip, err := s.Tip()
			if err != nil {
				t.Fatal(err)
			}
			if tip.Height != 2 || tip.ID != fork {
				t.Fatalf("the UTXO tip did not come back to the fork: %+v", tip)
			}
			id, err := s.BlockIDAt(tip.Height)
			if err != nil || id != tip.ID {
				t.Fatalf("the UTXO tip is still off the active branch: %x at %d (%v)", id[:4], tip.Height, err)
			}

			// A second call is a no-op: a healthy datadir must not be touched.
			if err := s.ReconcileUTXOToIndex(); err != nil {
				t.Fatalf("second reconcile: %v", err)
			}
			if tip2, err := s.Tip(); err != nil || tip2 != tip {
				t.Fatalf("reconcile moved a healthy tip: %+v -> %+v (%v)", tip, tip2, err)
			}
		})
	}
}
