package chain

import (
	"errors"
	"testing"
)

// poolSetup prepares a UTXO set with one output to a key, and a mempool.
func poolSetup(t *testing.T) (*Mempool, *UTXOSet, *[32]byte, []byte, Outpoint, Coin) {
	t.Helper()
	sk, pk := testKey(t, 41)
	u := NewUTXOSet()
	op := Outpoint{TxID: [32]byte{200}, Index: 0}
	coin := Coin{Value: 1_000_000, Script: pk, Height: 1}
	u.AddCoin(op, coin)
	return NewMempool(RegTest), u, sk, pk, op, coin
}

func signedSpend(t *testing.T, sk *[32]byte, op Outpoint, coin Coin, outs []TxOut) *Tx {
	t.Helper()
	tx := &Tx{Version: 1, Inputs: []TxIn{{PrevTxID: op.TxID, PrevIndex: op.Index}}, Outputs: outs}
	sig, err := SignInput(tx, 0, []Coin{coin}, sk, RegTest.At(1))
	if err != nil {
		t.Fatal(err)
	}
	tx.Witness = []Witness{{Items: [][]byte{sig}}}
	return tx
}

func TestMempoolAcceptsValidTx(t *testing.T) {
	m, u, sk, pk, op, coin := poolSetup(t)
	tx := signedSpend(t, sk, op, coin, []TxOut{{Value: 900_000, Script: pk}})

	e, err := m.Accept(tx, u, 10)
	if err != nil {
		t.Fatalf("a valid transaction was refused: %v", err)
	}
	if e.Fee != 100_000 {
		t.Fatalf("the fee should be 100 000, it is %d", e.Fee)
	}
	if m.Len() != 1 {
		t.Fatalf("the pool should hold 1 transaction, it holds %d", m.Len())
	}
	// The same transaction a second time.
	if _, err := m.Accept(tx, u, 10); !errors.Is(err, ErrMempoolDuplicate) {
		t.Fatalf("a duplicate should give ErrMempoolDuplicate, it gave %v", err)
	}
}

// Two transactions on the same input — the second is not accepted.
func TestMempoolRejectsDoubleSpend(t *testing.T) {
	m, u, sk, pk, op, coin := poolSetup(t)
	first := signedSpend(t, sk, op, coin, []TxOut{{Value: 900_000, Script: pk}})
	if _, err := m.Accept(first, u, 10); err != nil {
		t.Fatal(err)
	}
	second := signedSpend(t, sk, op, coin, []TxOut{{Value: 800_000, Script: pk}})
	if _, err := m.Accept(second, u, 10); !errors.Is(err, ErrMempoolConflict) {
		t.Fatalf("a double spend should give ErrMempoolConflict, it gave %v", err)
	}
}

// Policy: a low fee and dust outputs are not accepted. Consensus permits them, the mempool does
// not — it keeps the UTXO set small and spam expensive.
func TestMempoolPolicy(t *testing.T) {
	m, u, sk, pk, op, coin := poolSetup(t)

	// A fee of 0 → below the minimum.
	noFee := signedSpend(t, sk, op, coin, []TxOut{{Value: coin.Value, Script: pk}})
	if _, err := m.Accept(noFee, u, 10); !errors.Is(err, ErrFeeTooLow) {
		t.Fatalf("a zero fee should give ErrFeeTooLow, it gave %v", err)
	}

	// A dust output.
	dust := signedSpend(t, sk, op, coin, []TxOut{
		{Value: DustLimit - 1, Script: pk}, {Value: 900_000, Script: pk}})
	if _, err := m.Accept(dust, u, 10); !errors.Is(err, ErrDustOutput) {
		t.Fatalf("a dust output should give ErrDustOutput, it gave %v", err)
	}

	// A non-existent input.
	ghost := signedSpend(t, sk, Outpoint{TxID: [32]byte{1}, Index: 0}, coin,
		[]TxOut{{Value: 900_000, Script: pk}})
	if _, err := m.Accept(ghost, u, 10); !errors.Is(err, ErrCoinMissing) {
		t.Fatalf("a non-existent input should give ErrCoinMissing, it gave %v", err)
	}

	// An invalid signature.
	bad := signedSpend(t, sk, op, coin, []TxOut{{Value: 900_000, Script: pk}})
	bad.Witness[0].Items[0][5] ^= 0xff
	if _, err := m.Accept(bad, u, 10); !errors.Is(err, ErrBadTxSignature) {
		t.Fatalf("an invalid signature should give ErrBadTxSignature, it gave %v", err)
	}

	// A coinbase does not belong in the mempool.
	cb := coinbaseTx(10, 100, RegTest)
	if _, err := m.Accept(cb, u, 10); !errors.Is(err, ErrCoinbaseInPool) {
		t.Fatalf("a coinbase should give ErrCoinbaseInPool, it gave %v", err)
	}
}

// An immature coinbase cannot be spent through the mempool either.
func TestMempoolRespectsCoinbaseMaturity(t *testing.T) {
	m, u, sk, pk, _, _ := poolSetup(t)
	cbOp := Outpoint{TxID: [32]byte{111}, Index: 0}
	cbCoin := Coin{Value: 1_000_000, Script: pk, Height: 100, Coinbase: true}
	u.AddCoin(cbOp, cbCoin)

	tx := signedSpend(t, sk, cbOp, cbCoin, []TxOut{{Value: 900_000, Script: pk}})
	if _, err := m.Accept(tx, u, 150); !errors.Is(err, ErrCoinImmature) {
		t.Fatalf("an immature coinbase should give ErrCoinImmature, it gave %v", err)
	}
	if _, err := m.Accept(tx, u, 100+CoinbaseMaturity); err != nil {
		t.Fatalf("after maturing the transaction should pass: %v", err)
	}
}

// A transaction may spend an output of another transaction from the mempool; they have to go into the block in the right order.
func TestMempoolChainsAndTemplateOrder(t *testing.T) {
	m, u, sk, pk, op, coin := poolSetup(t)
	parent := signedSpend(t, sk, op, coin, []TxOut{{Value: 900_000, Script: pk}})
	if _, err := m.Accept(parent, u, 10); err != nil {
		t.Fatal(err)
	}

	parentOut := Coin{Value: 900_000, Script: pk, Height: 10}
	child := signedSpend(t, sk, Outpoint{TxID: parent.TxID(), Index: 0}, parentOut,
		[]TxOut{{Value: 800_000, Script: pk}})
	if _, err := m.Accept(child, u, 10); err != nil {
		t.Fatalf("a chained transaction was refused: %v", err)
	}

	sel := m.Select(MaxBlockBytes-MaxCoinbaseBytes, MaxBlockStorageMass)
	if len(sel) != 2 {
		t.Fatalf("2 transactions should have gone into the template, %d did", len(sel))
	}
	if sel[0].TxID() != parent.TxID() {
		t.Fatal("the parent has to come before the child in the block")
	}
}

// The template takes the highest fee first and fits inside the limit.
func TestTemplateRespectsLimitAndFeeOrder(t *testing.T) {
	m, u, sk, pk, _, _ := poolSetup(t)
	// Three independent outputs with different fees.
	fees := []int64{5_000, 50_000, 500_000}
	for i, fee := range fees {
		op := Outpoint{TxID: [32]byte{byte(50 + i)}, Index: 0}
		coin := Coin{Value: 1_000_000, Script: pk, Height: 1}
		u.AddCoin(op, coin)
		tx := signedSpend(t, sk, op, coin, []TxOut{{Value: 1_000_000 - fee, Script: pk}})
		if _, err := m.Accept(tx, u, 10); err != nil {
			t.Fatalf("fee %d: %v", fee, err)
		}
	}
	sel := m.Select(MaxBlockBytes-MaxCoinbaseBytes, MaxBlockStorageMass)
	if len(sel) != 3 {
		t.Fatalf("expected 3 transactions, got %d", len(sel))
	}
	// The first has to be the one with the highest fee.
	first := sel[0]
	var firstFee int64 = 1_000_000 - first.Outputs[0].Value
	if firstFee != 500_000 {
		t.Fatalf("the first in the template should be the most expensive transaction, its fee is %d", firstFee)
	}
	// A small limit lets only one through.
	small := m.Select(len(first.Serialize())+10, MaxBlockStorageMass)
	if len(small) != 1 {
		t.Fatalf("1 transaction should fit into the small limit, %d did", len(small))
	}
}

// Once it is in a block, a transaction disappears from the pool — including one that conflicted with it.
func TestRemoveConfirmedDropsConflicts(t *testing.T) {
	m, u, sk, pk, op, coin := poolSetup(t)
	mine := signedSpend(t, sk, op, coin, []TxOut{{Value: 900_000, Script: pk}})
	if _, err := m.Accept(mine, u, 10); err != nil {
		t.Fatal(err)
	}
	// A different transaction spending the same input made it into the block.
	other := signedSpend(t, sk, op, coin, []TxOut{{Value: 850_000, Script: pk}})
	blk := blockWith(11, []*Tx{coinbaseTx(11, 0, RegTest), other})

	m.RemoveConfirmed(blk)
	if m.Len() != 0 {
		t.Fatalf("the conflicting transaction should have disappeared from the pool, %d remained", m.Len())
	}
}

// A transaction no block has room for is refused at the door: accepted, it would be relayed to every
// peer and sit in the pool until it expired, never mined.
func TestMempoolRefusesATransactionNoBlockCanHold(t *testing.T) {
	m, u, sk, pk, _, _ := poolSetup(t)
	op := Outpoint{TxID: [32]byte{201}, Index: 0}
	coin := Coin{Value: 1000 * POWM, Script: pk, Height: 1}
	u.AddCoin(op, coin)
	var outs []TxOut
	for len(outs)*MaxScriptBytes <= MaxBlockBytes-MaxCoinbaseBytes {
		outs = append(outs, TxOut{Value: POWM, ScriptVersion: 1, Script: make([]byte, MaxScriptBytes)})
	}
	tx := signedSpend(t, sk, op, coin, outs)
	if err := tx.CheckSanity(); err != nil {
		t.Fatalf("setup: the transaction should be sane on its own: %v", err)
	}
	if _, err := m.Accept(tx, u, 10); !errors.Is(err, ErrTxTooBig) {
		t.Fatalf("a %d B transaction: %v, want ErrTxTooBig", len(tx.Serialize()), err)
	}
}

// After a reorg the confirmed state is another one. What the pool holds is checked against it again:
// a transaction whose coin is gone falls out and takes its children along, the rest stays.
func TestMempoolRevalidateDropsWhatNoLongerHolds(t *testing.T) {
	m, u, sk, pk, op, coin := poolSetup(t)
	other := Outpoint{TxID: [32]byte{202}, Index: 0}
	otherCoin := Coin{Value: 1_000_000, Script: pk, Height: 1}
	u.AddCoin(other, otherCoin)

	parent := signedSpend(t, sk, op, coin, []TxOut{{Value: 900_000, Script: pk}})
	if _, err := m.Accept(parent, u, 10); err != nil {
		t.Fatal(err)
	}
	parentOut := Outpoint{TxID: parent.TxID(), Index: 0}
	child := signedSpend(t, sk, parentOut, Coin{Value: 900_000, Script: pk, Height: 10},
		[]TxOut{{Value: 800_000, Script: pk}})
	if _, err := m.Accept(child, u, 10); err != nil {
		t.Fatal(err)
	}
	lone := signedSpend(t, sk, other, otherCoin, []TxOut{{Value: 900_000, Script: pk}})
	if _, err := m.Accept(lone, u, 10); err != nil {
		t.Fatal(err)
	}

	// Nothing changed: nothing falls out.
	if dropped := m.Revalidate(u, 10); dropped != 0 || m.Len() != 3 {
		t.Fatalf("an unchanged state dropped %d, %d left", dropped, m.Len())
	}

	// The new branch never had the coin the parent spends.
	after := NewUTXOSet()
	after.AddCoin(other, otherCoin)
	if dropped := m.Revalidate(after, 10); dropped != 2 {
		t.Fatalf("dropped %d transactions, want the parent and its child", dropped)
	}
	if _, ok := m.Get(lone.TxID()); !ok || m.Len() != 1 {
		t.Fatal("the transaction that still holds did not stay")
	}
	if sel := m.Select(MaxBlockBytes-MaxCoinbaseBytes, MaxBlockStorageMass-CoinbaseStorageMassReserve); len(sel) != 1 {
		t.Fatalf("a template would carry %d transactions, want 1", len(sel))
	}
}
