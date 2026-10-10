package chain

import (
	"errors"
	"testing"
)

// chainFrom builds a chain of n transactions, each spending the previous one's output.
func chainFrom(t *testing.T, sk *[32]byte, pk []byte, op Outpoint, coin Coin, n int, fee int64) []*Tx {
	t.Helper()
	var txs []*Tx
	for range n {
		out := coin.Value - fee
		tx := signedSpend(t, sk, op, coin, []TxOut{{Value: out, Script: pk}})
		txs = append(txs, tx)
		op = Outpoint{TxID: tx.TxID(), Index: 0}
		coin = Coin{Value: out, Script: pk}
	}
	return txs
}

func TestMempoolChainLimit(t *testing.T) {
	m, u, sk, pk, op, coin := poolSetup(t)
	txs := chainFrom(t, sk, pk, op, coin, MaxChain+1, 500)
	for i, tx := range txs[:MaxChain] {
		if _, err := m.Accept(tx, u, 200); err != nil {
			t.Fatalf("transaction %d in the chain: %v", i+1, err)
		}
	}
	if _, err := m.Accept(txs[MaxChain], u, 200); !errors.Is(err, ErrChainTooLong) {
		t.Fatalf("transaction %d in the chain: %v, expected ErrChainTooLong", MaxChain+1, err)
	}
}

// A conflict with a confirmed block throws out the parent and its whole tree — otherwise the
// template would include a child without its parent and the block would be invalid.
func TestMempoolConflictRemovesDescendants(t *testing.T) {
	m, u, sk, pk, op, coin := poolSetup(t)
	txs := chainFrom(t, sk, pk, op, coin, 3, 500)
	for _, tx := range txs {
		if _, err := m.Accept(tx, u, 200); err != nil {
			t.Fatal(err)
		}
	}
	// The block contains another transaction spending the same coin as txs[0].
	rival := signedSpend(t, sk, op, coin, []TxOut{{Value: coin.Value - 1000, Script: pk}})
	m.RemoveConfirmed(&Block{Txs: []*Tx{rival}})
	if m.Len() != 0 {
		t.Fatalf("%d transactions remained in the pool after the conflict (children without a parent)", m.Len())
	}
	if got := m.Select(1<<20, MaxBlockStorageMass); len(got) != 0 {
		t.Fatalf("the template picked %d transactions without a parent", len(got))
	}
}

// A confirmed parent leaves on its own, the child stays (its input is now in the UTXO set).
func TestMempoolConfirmedParentKeepsChild(t *testing.T) {
	m, u, sk, pk, op, coin := poolSetup(t)
	txs := chainFrom(t, sk, pk, op, coin, 2, 500)
	for _, tx := range txs {
		m.Accept(tx, u, 200)
	}
	m.RemoveConfirmed(&Block{Txs: []*Tx{txs[0]}})
	if _, ok := m.Get(txs[1].TxID()); !ok || m.Len() != 1 {
		t.Fatal("the child of a confirmed parent disappeared from the pool")
	}
}

// CPFP: a cheap parent + an expensive child. When there is room for only one package, the one with
// the higher rate is picked — and the parent comes before the child in the block.
func TestMempoolSelectPackages(t *testing.T) {
	m, u, sk, pk, op, coin := poolSetup(t)
	cheap := signedSpend(t, sk, op, coin, []TxOut{{Value: coin.Value - 300, Script: pk}})
	child := signedSpend(t, sk, Outpoint{TxID: cheap.TxID()}, Coin{Value: coin.Value - 300, Script: pk},
		[]TxOut{{Value: coin.Value - 300 - 20_000, Script: pk}})
	for _, tx := range []*Tx{child, cheap} { // the arrival order does not matter, the child only goes after the parent
		m.Accept(tx, u, 200)
	}
	if m.Len() != 1 {
		t.Fatalf("a child before its parent should not have been accepted, the pool holds %d", m.Len())
	}
	if _, err := m.Accept(child, u, 200); err != nil {
		t.Fatal(err)
	}
	got := m.Select(1<<20, MaxBlockStorageMass)
	if len(got) != 2 || got[0].TxID() != cheap.TxID() || got[1].TxID() != child.TxID() {
		t.Fatalf("the template: %d transactions, parent first = %v", len(got), len(got) > 0 && got[0].TxID() == cheap.TxID())
	}
	// Room for only one transaction: the package (parent+child) does not fit, and the child alone must not go.
	one := len(cheap.Serialize()) + 1
	for _, tx := range m.Select(one, MaxBlockStorageMass) {
		if tx.TxID() == child.TxID() {
			t.Fatal("the child was picked without its parent")
		}
	}
}
