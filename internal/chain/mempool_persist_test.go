package chain

import (
	"bytes"
	"errors"
	"testing"
	"time"
)

// Dumping and loading: after a restart the pool has to hold the same thing it held before.
func TestMempoolDumpLoadRoundTrip(t *testing.T) {
	m, u, sk, pk, op, coin := poolSetup(t)
	parent := signedSpend(t, sk, op, coin, []TxOut{{Value: 900_000, Script: pk}})
	if _, err := m.Accept(parent, u, 10); err != nil {
		t.Fatal(err)
	}
	// A child, so the order is exercised too: on loading the parent has to pass first, otherwise the
	// child would look like an orphan and the whole chain would be lost.
	pid := parent.TxID()
	childCoin := Coin{Value: 900_000, Script: pk, Height: 10}
	child := signedSpend(t, sk, Outpoint{TxID: pid, Index: 0}, childCoin,
		[]TxOut{{Value: 800_000, Script: pk}})
	if _, err := m.Accept(child, u, 10); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	count, err := m.Dump(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("%d transactions written, expected 2", count)
	}

	fresh := NewMempool(RegTest)
	loaded, dropped, err := fresh.Load(bytes.NewReader(buf.Bytes()), u, 10, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if loaded != 2 || dropped != 0 {
		t.Fatalf("%d loaded, %d dropped — expected 2 and 0", loaded, dropped)
	}
	for _, want := range [][32]byte{pid, child.TxID()} {
		if _, ok := fresh.Get(want); !ok {
			t.Errorf("the transaction %x was not found in the pool after loading", want[:6])
		}
	}
	if fresh.Bytes() != m.Bytes() {
		t.Errorf("the pool size %d != %d", fresh.Bytes(), m.Bytes())
	}
}

// The time of acceptance has to be carried over. If a restart made it young again, a transaction
// would stay in the pool forever: expiry would be counted from scratch at every restart.
func TestMempoolLoadKeepsAddedTime(t *testing.T) {
	m, u, sk, pk, op, coin := poolSetup(t)
	tx := signedSpend(t, sk, op, coin, []TxOut{{Value: 900_000, Script: pk}})
	e, err := m.Accept(tx, u, 10)
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-30 * time.Hour).Truncate(time.Second)
	e.Added = old

	var buf bytes.Buffer
	if _, err := m.Dump(&buf); err != nil {
		t.Fatal(err)
	}
	fresh := NewMempool(RegTest)
	if _, _, err := fresh.Load(bytes.NewReader(buf.Bytes()), u, 10, time.Now()); err != nil {
		t.Fatal(err)
	}
	got, ok := fresh.Get(tx.TxID())
	if !ok {
		t.Fatal("the transaction was not loaded")
	}
	if !got.Added.Equal(old) {
		t.Fatalf("the time of acceptance is %v, expected %v", got.Added, old)
	}
}

// A transaction that sat in the file longer than MempoolExpiry is not loaded.
func TestMempoolLoadDropsExpired(t *testing.T) {
	m, u, sk, pk, op, coin := poolSetup(t)
	tx := signedSpend(t, sk, op, coin, []TxOut{{Value: 900_000, Script: pk}})
	e, err := m.Accept(tx, u, 10)
	if err != nil {
		t.Fatal(err)
	}
	e.Added = time.Now().Add(-MempoolExpiry - time.Hour)

	var buf bytes.Buffer
	if _, err := m.Dump(&buf); err != nil {
		t.Fatal(err)
	}
	fresh := NewMempool(RegTest)
	loaded, dropped, err := fresh.Load(bytes.NewReader(buf.Bytes()), u, 10, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if loaded != 0 || dropped != 1 {
		t.Fatalf("%d loaded, %d dropped — expected 0 and 1", loaded, dropped)
	}
}

// The file is input like any other: a transaction in it gets no privilege. The signature is
// verified again, so a planted transaction does not get into the pool.
func TestMempoolLoadVerifiesSignatures(t *testing.T) {
	m, u, sk, pk, op, coin := poolSetup(t)
	tx := signedSpend(t, sk, op, coin, []TxOut{{Value: 900_000, Script: pk}})
	if _, err := m.Accept(tx, u, 10); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if _, err := m.Dump(&buf); err != nil {
		t.Fatal(err)
	}
	raw := buf.Bytes()

	// Flip one byte of the signature (it is at the end of the transaction, in the witness).
	raw[len(raw)-1] ^= 0xFF

	fresh := NewMempool(RegTest)
	loaded, dropped, err := fresh.Load(bytes.NewReader(raw), u, 10, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if loaded != 0 || dropped != 1 {
		t.Fatalf("a planted transaction passed: %d loaded, %d dropped", loaded, dropped)
	}
}

func TestMempoolLoadRejectsGarbage(t *testing.T) {
	_, u, _, _, _, _ := poolSetup(t)
	for name, raw := range map[string][]byte{
		"empty":         {},
		"foreign magic": []byte("XXXXXXXX\x00\x00\x00\x00"),
		"short":         append([]byte("POWMMP01"), 1, 0, 0, 0),
	} {
		fresh := NewMempool(RegTest)
		if _, _, err := fresh.Load(bytes.NewReader(raw), u, 10, time.Now()); !errors.Is(err, ErrMempoolDumpFormat) {
			t.Errorf("%s: expected ErrMempoolDumpFormat, got %v", name, err)
		}
		if fresh.Len() != 0 {
			t.Errorf("%s: the pool should be empty, it holds %d", name, fresh.Len())
		}
	}
}

// A header reporting a nonsensical count must not cause an allocation — not even an attempt to read.
func TestMempoolLoadRejectsAbsurdCount(t *testing.T) {
	_, u, _, _, _, _ := poolSetup(t)
	raw := append([]byte("POWMMP01"), 0xFF, 0xFF, 0xFF, 0xFF)
	fresh := NewMempool(RegTest)
	if _, _, err := fresh.Load(bytes.NewReader(raw), u, 10, time.Now()); !errors.Is(err, ErrMempoolDumpFormat) {
		t.Fatalf("expected ErrMempoolDumpFormat, got %v", err)
	}
}

// Expiry takes the descendants too: a child without its parent is invalid.
func TestMempoolExpireTakesDescendants(t *testing.T) {
	m, u, sk, pk, op, coin := poolSetup(t)
	parent := signedSpend(t, sk, op, coin, []TxOut{{Value: 900_000, Script: pk}})
	pe, err := m.Accept(parent, u, 10)
	if err != nil {
		t.Fatal(err)
	}
	pid := parent.TxID()
	child := signedSpend(t, sk, Outpoint{TxID: pid, Index: 0},
		Coin{Value: 900_000, Script: pk, Height: 10}, []TxOut{{Value: 800_000, Script: pk}})
	if _, err := m.Accept(child, u, 10); err != nil {
		t.Fatal(err)
	}
	// Only the parent is old; the child has to leave with it.
	pe.Added = time.Now().Add(-MempoolExpiry - time.Minute)

	if dropped := m.ExpireBefore(time.Now().Add(-MempoolExpiry)); dropped != 2 {
		t.Fatalf("%d removed, expected 2 (the parent and the child)", dropped)
	}
	if m.Len() != 0 || m.Bytes() != 0 {
		t.Fatalf("the pool is not empty: %d transactions, %d bytes", m.Len(), m.Bytes())
	}
}
