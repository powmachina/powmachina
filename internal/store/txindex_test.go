package store

import (
	"errors"
	"path/filepath"
	"testing"
)

// Without the index on, a transaction cannot be found — and that has to be distinguishable from the
// transaction not existing. The caller depends on it: "I have no index" is a configuration error,
// "I do not have the transaction" is a valid answer.
func TestTxIndexOffIsDistinguishable(t *testing.T) {
	s, err := OpenMem()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if s.HasTxIndex() {
		t.Fatal("the index should not have been enabled")
	}
	blocks := testChain(3)
	for _, b := range blocks {
		if err := s.PutBlock(b); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ApplyBlockDirect(b); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.TxLocation(blocks[1].Txs[0].TxID()); !errors.Is(err, ErrNoTxIndex) {
		t.Fatalf("expected ErrNoTxIndex, got %v", err)
	}
}

func TestTxIndexFindsEveryTransaction(t *testing.T) {
	s, err := OpenMemWith(Options{TxIndex: true})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	blocks := testChain(12)
	for _, b := range blocks {
		if err := s.PutBlock(b); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ApplyBlockDirect(b); err != nil {
			t.Fatal(err)
		}
	}

	for h, b := range blocks {
		for i, tx := range b.Txs {
			txid := tx.TxID()
			loc, err := s.TxLocation(txid)
			if err != nil {
				t.Fatalf("height %d, tx %d: %v", h, i, err)
			}
			if loc.BlockID != b.Header.ID() || loc.Index != uint32(i) {
				t.Fatalf("height %d, tx %d: the index points elsewhere (%x/%d)",
					h, i, loc.BlockID[:6], loc.Index)
			}
			got, gotBlk, err := s.Tx(txid)
			if err != nil {
				t.Fatalf("Tx(%x): %v", txid[:6], err)
			}
			if gotID := got.TxID(); gotID != txid {
				t.Fatalf("Tx returned a different transaction: %x != %x", gotID[:6], txid[:6])
			}
			if gotBlk.Header.Height != b.Header.Height {
				t.Fatalf("Tx returned a block from height %d, expected %d",
					gotBlk.Header.Height, b.Header.Height)
			}
		}
	}
}

// This is the important test: the index has to be unwound together with the block.
//
// If a record stayed after a revert, the node would claim a transaction from an unwound branch is in
// the chain — and that is exactly what breaks explorers built on a separate database.
// (iquidus: "orphaned blocks… reports them against related address's balances").
func TestTxIndexUnwindsWithReorg(t *testing.T) {
	s, err := OpenMemWith(Options{TxIndex: true})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	blocks := testChain(6)
	for _, b := range blocks {
		if err := s.PutBlock(b); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ApplyBlockDirect(b); err != nil {
			t.Fatal(err)
		}
	}
	orphan := blocks[5]
	orphanTx := orphan.Txs[0].TxID()
	kept := blocks[4].Txs[0].TxID()

	if _, err := s.TxLocation(orphanTx); err != nil {
		t.Fatalf("it should have been found before the revert: %v", err)
	}
	if err := s.RevertTip(); err != nil {
		t.Fatal(err)
	}

	if _, err := s.TxLocation(orphanTx); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the transaction from the unwound block is still found (%v) — the index did not unwind", err)
	}
	if _, err := s.TxLocation(kept); err != nil {
		t.Fatalf("the transaction from the block that stayed was lost: %v", err)
	}

	// After applying it again it has to come back.
	if _, err := s.ApplyBlockDirect(orphan); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TxLocation(orphanTx); err != nil {
		t.Fatalf("after applying again it was not found: %v", err)
	}
}

// Turning the index on over an existing directory means walking the chain from scratch. Without that
// the index would cover only newer blocks and older transactions would silently "not exist".
func TestReindexCoversWholeChain(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "db")

	s, err := Open(dir) // without the index
	if err != nil {
		t.Fatal(err)
	}
	blocks := testChain(8)
	for _, b := range blocks {
		if err := s.PutBlock(b); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ApplyBlockDirect(b); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()

	s2, err := OpenWith(dir, Options{TxIndex: true})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()

	if s2.TxIndexReady() {
		t.Fatal("the index marked itself ready without a reindex")
	}
	// The index does not know the older transactions yet.
	if _, err := s2.TxLocation(blocks[0].Txs[0].TxID()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("before the reindex it should not have been found: %v", err)
	}

	if err := s2.ReindexTxs(nil); err != nil {
		t.Fatal(err)
	}
	if !s2.TxIndexReady() {
		t.Fatal("the readiness mark is missing after the reindex")
	}
	for h, b := range blocks {
		if _, err := s2.TxLocation(b.Txs[0].TxID()); err != nil {
			t.Fatalf("the transaction from height %d is missing after the reindex: %v", h, err)
		}
	}
}

// A header without a body is a legitimate state — the genesis is created as a header only and while
// downloading, bodies arrive out of order. The reindex must not crash on that.
//
// A node with `-txindex` over a fresh datadir is the plainest case: it must not crash at startup on
// "reindex of height 0: the record does not exist".
func TestReindexToleratesMissingBody(t *testing.T) {
	s, err := OpenMemWith(Options{TxIndex: true})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	blocks := testChain(4)
	// The genesis as a header only, without a body — exactly what InitGenesis does.
	if err := s.PutHeader(blocks[0].Header); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyBlockDirect(blocks[0]); err != nil {
		t.Fatal(err)
	}
	for _, b := range blocks[1:] {
		if err := s.PutBlock(b); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ApplyBlockDirect(b); err != nil {
			t.Fatal(err)
		}
	}

	if err := s.ReindexTxs(nil); err != nil {
		t.Fatalf("the reindex must not crash on a missing body: %v", err)
	}
	// The blocks with a body have to be in the index.
	for h, b := range blocks[1:] {
		if _, err := s.TxLocation(b.Txs[0].TxID()); err != nil {
			t.Fatalf("the transaction from height %d is missing: %v", h+1, err)
		}
	}
}
