package chain

import (
	"errors"
	"testing"
	"time"
)

// orphanTx builds a transaction that spends a given outpoint. The signature does not matter here:
// the orphan pool does not verify transactions, it only holds them until a parent turns up.
func orphanTx(spends Outpoint, value int64) *Tx {
	return &Tx{Version: 1,
		Inputs:  []TxIn{{PrevTxID: spends.TxID, PrevIndex: spends.Index}},
		Outputs: []TxOut{{Value: value, Script: make([]byte, 32)}},
		Witness: []Witness{{Items: [][]byte{make([]byte, 64)}}}}
}

func TestOrphanPoolHoldsAndReleasesByParent(t *testing.T) {
	p := NewOrphanPool()
	parent := Outpoint{TxID: [32]byte{7}, Index: 0}
	child := orphanTx(parent, 900_000)

	parents := p.Add(child, []Outpoint{parent}, time.Now())
	if len(parents) != 1 || parents[0] != parent.TxID {
		t.Fatalf("Add should return the parent, it returned %v", parents)
	}
	if p.Len() != 1 || p.Bytes() == 0 {
		t.Fatalf("the pool should hold 1 transaction, it holds %d (%d bytes)", p.Len(), p.Bytes())
	}
	if !p.Has(child.TxID()) {
		t.Error("Has has to know about the transaction — otherwise the node asks another peer for it again")
	}

	// A foreign parent releases nothing.
	if got := p.TakeChildren([32]byte{99}); len(got) != 0 {
		t.Fatalf("a foreign parent released %d transactions", len(got))
	}
	got := p.TakeChildren(parent.TxID)
	if len(got) != 1 || got[0].TxID() != child.TxID() {
		t.Fatalf("expected one child, got %d", len(got))
	}
	if p.Len() != 0 || p.Bytes() != 0 {
		t.Fatalf("after taking it out the pool should be empty, it holds %d (%d bytes)", p.Len(), p.Bytes())
	}
	// The second time there is nothing left to take.
	if got := p.TakeChildren(parent.TxID); len(got) != 0 {
		t.Fatalf("a second take returned %d transactions", len(got))
	}
}

// A transaction with no missing inputs does not belong in the orphan pool — it would stay there
// forever, because the parent it waits for would never arrive.
func TestOrphanPoolRejectsNonOrphans(t *testing.T) {
	p := NewOrphanPool()
	tx := orphanTx(Outpoint{TxID: [32]byte{7}}, 900_000)
	if parents := p.Add(tx, nil, time.Now()); len(parents) != 0 {
		t.Fatal("a transaction with no missing inputs was filed")
	}
	if p.Len() != 0 {
		t.Fatalf("the pool should be empty, it holds %d", p.Len())
	}
	// The same transaction twice is once.
	op := Outpoint{TxID: [32]byte{7}, Index: 0}
	p.Add(tx, []Outpoint{op}, time.Now())
	p.Add(tx, []Outpoint{op}, time.Now())
	if p.Len() != 1 {
		t.Fatalf("a duplicate was filed a second time: %d in the pool", p.Len())
	}
}

// The pool is a cap, not a warehouse: flooding must not let it grow. It is the only place in the
// node where unverified data from strangers sits.
func TestOrphanPoolStaysBounded(t *testing.T) {
	p := NewOrphanPool()
	now := time.Now()
	for i := 0; i < MaxOrphanTxs*3; i++ {
		op := Outpoint{TxID: [32]byte{byte(i), byte(i >> 8), 1}, Index: 0}
		p.Add(orphanTx(op, 900_000), []Outpoint{op}, now)
	}
	if p.Len() > MaxOrphanTxs {
		t.Fatalf("the pool holds %d transactions, the cap is %d", p.Len(), MaxOrphanTxs)
	}
	if p.Bytes() > MaxOrphanBytes {
		t.Fatalf("the pool holds %d bytes, the cap is %d", p.Bytes(), MaxOrphanBytes)
	}
}

// When the parent never arrived, the child is dropped after OrphanExpiry — otherwise the pool
// would hold transactions whose parent does not exist until a restart.
func TestOrphanPoolExpires(t *testing.T) {
	p := NewOrphanPool()
	stale := Outpoint{TxID: [32]byte{7}, Index: 0}
	p.Add(orphanTx(stale, 900_000), []Outpoint{stale}, time.Now().Add(-OrphanExpiry-time.Minute))
	if p.Len() != 1 {
		t.Fatalf("the pool should hold 1 transaction, it holds %d", p.Len())
	}

	if dropped := p.ExpireBefore(time.Now().Add(-OrphanExpiry)); dropped != 1 {
		t.Fatalf("%d dropped, expected 1", dropped)
	}
	if p.Len() != 0 || p.Bytes() != 0 {
		t.Fatalf("the pool should be empty, it holds %d (%d bytes)", p.Len(), p.Bytes())
	}
	// After expiry nobody may claim its parent any more — otherwise `waiting` would keep a reference
	// to a transaction that is not in the pool.
	if got := p.TakeChildren(stale.TxID); len(got) != 0 {
		t.Fatalf("an expired transaction was still released (%d)", len(got))
	}
}

// Filing a new transaction sweeps the old ones by itself — without that the pool would only be
// cleaned once every five minutes and could be filled with expired ones in between.
func TestOrphanPoolAddSweepsStale(t *testing.T) {
	p := NewOrphanPool()
	stale := Outpoint{TxID: [32]byte{7}, Index: 0}
	p.Add(orphanTx(stale, 900_000), []Outpoint{stale}, time.Now().Add(-OrphanExpiry-time.Minute))

	fresh := Outpoint{TxID: [32]byte{8}, Index: 0}
	p.Add(orphanTx(fresh, 900_000), []Outpoint{fresh}, time.Now())

	if p.Len() != 1 {
		t.Fatalf("only the new one should be left in the pool, there are %d", p.Len())
	}
	if got := p.TakeChildren(fresh.TxID); len(got) != 1 {
		t.Fatalf("the new transaction was not found (%d)", len(got))
	}
}

// Accept has to return the list of missing inputs — without it the orphan pool has nothing to wait
// for and the node has nothing to ask for.
func TestAcceptReportsMissingInputs(t *testing.T) {
	m, u, sk, pk, op, coin := poolSetup(t)
	// The transaction spends two inputs: one exists, the other does not.
	ghost := Outpoint{TxID: [32]byte{250}, Index: 3}
	tx := &Tx{Version: 1,
		Inputs: []TxIn{{PrevTxID: op.TxID, PrevIndex: op.Index},
			{PrevTxID: ghost.TxID, PrevIndex: ghost.Index}},
		Outputs: []TxOut{{Value: 900_000, Script: pk}}}
	sig, err := SignInput(tx, 0, []Coin{coin, coin}, sk, RegTest.At(1))
	if err != nil {
		t.Fatal(err)
	}
	tx.Witness = []Witness{{Items: [][]byte{sig}}, {Items: [][]byte{sig}}}

	_, err = m.Accept(tx, u, 10)
	var miss *MissingInputs
	if !errors.As(err, &miss) {
		t.Fatalf("expected *MissingInputs, got %v", err)
	}
	if len(miss.Outpoints) != 1 || miss.Outpoints[0] != ghost {
		t.Fatalf("expected the missing %v, got %v", ghost, miss.Outpoints)
	}
	// The old way of checking has to keep working — a caller without an orphan pool sees it as
	// before.
	if !errors.Is(err, ErrCoinMissing) {
		t.Error("errors.Is(err, ErrCoinMissing) stopped holding")
	}
}
