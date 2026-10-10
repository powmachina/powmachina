package chain

import (
	"reflect"
	"testing"
)

// A reorg stands and falls on the state being restored exactly. This is that test.
func TestApplyUndoRestoresExactState(t *testing.T) {
	p := RegTest
	u := NewUTXOSet()
	sk, pk := testKey(t, 5)
	prev := Outpoint{TxID: [32]byte{42}, Index: 0}
	prevCoin := Coin{Value: 10 * POWM, Script: pk, Height: 5}
	u.AddCoin(prev, prevCoin)
	before := u.Snapshot()

	h := uint64(10)
	spend := &Tx{Version: 1,
		Inputs:  []TxIn{{PrevTxID: prev.TxID, PrevIndex: 0}},
		Outputs: []TxOut{{Value: 6 * POWM, Script: pk}, {Value: 3 * POWM, Script: pk}},
	}
	sig, err := SignInput(spend, 0, []Coin{prevCoin}, sk)
	if err != nil {
		t.Fatal(err)
	}
	spend.Witness = []Witness{{Items: [][]byte{sig}}}
	b := blockWith(h, []*Tx{coinbaseTx(h, MinerSubsidy(h), p), spend})
	if err := b.ValidateBlock(u, p); err != nil {
		t.Fatal(err)
	}

	undo, err := u.Apply(b)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := u.Get(prev); ok {
		t.Fatal("a spent output has to disappear from the UTXO set")
	}
	if u.Len() != 4 { // 2 from the coinbase (the dev fee + the miner) + 2 new ones
		t.Fatalf("after the block I expect 4 outputs, I have %d", u.Len())
	}

	u.Undo(undo)
	if !reflect.DeepEqual(u.Snapshot(), before) {
		t.Fatal("Undo did not restore exactly the original state")
	}
}

// Data outputs do not belong in the UTXO set — otherwise every "OP_RETURN" would inflate it forever.
func TestDataOutputsStayOutOfUTXO(t *testing.T) {
	p := RegTest
	u := NewUTXOSet()
	h := uint64(10)
	cb := coinbaseTx(h, MinerSubsidy(h), p)
	cb.Outputs = append(cb.Outputs, TxOut{Value: 0, ScriptVersion: ScriptVersionData,
		Script: []byte("an anchor to something")})
	b := blockWith(h, []*Tx{cb})
	if err := b.ValidateBlock(u, p); err != nil {
		t.Fatal(err)
	}
	if _, err := u.Apply(b); err != nil {
		t.Fatal(err)
	}
	if u.Len() != 2 {
		t.Fatalf("the data output should not have been stored; I expect 2 outputs, I have %d", u.Len())
	}
}

// A data output with a value or over the limit is invalid.
func TestDataOutputLimits(t *testing.T) {
	tx := &Tx{Version: 1, Inputs: []TxIn{{PrevTxID: [32]byte{1}, PrevIndex: 0}},
		Outputs: []TxOut{{Value: 1, ScriptVersion: ScriptVersionData, Script: []byte("x")}}}
	if err := tx.CheckSanity(); err != ErrTxBadScript {
		t.Fatalf("a data output with a value has to be invalid, got %v", err)
	}
	tx.Outputs[0].Value = 0
	tx.Outputs[0].Script = make([]byte, MaxDataScriptBytes+1)
	if err := tx.CheckSanity(); err != ErrTxBadScript {
		t.Fatalf("an oversized data output has to be invalid, got %v", err)
	}
	tx.Outputs[0].Script = make([]byte, MaxDataScriptBytes)
	tx.Outputs = append(tx.Outputs, TxOut{Value: 0, ScriptVersion: ScriptVersionData})
	if err := tx.CheckSanity(); err != ErrTxBadScript {
		t.Fatalf("two data outputs have to be invalid, got %v", err)
	}
}

// Script versions 1..15 are reserved for future types (the soft-fork path), higher ones are invalid.
func TestScriptVersionSpace(t *testing.T) {
	mk := func(v uint8) *Tx {
		return &Tx{Version: 1, Inputs: []TxIn{{PrevTxID: [32]byte{1}, PrevIndex: 0}},
			Outputs: []TxOut{{Value: 10, ScriptVersion: v, Script: []byte("s")}}}
	}
	for _, v := range []uint8{0, 1, 7, ScriptVersionMaxReserved} {
		if err := mk(v).CheckSanity(); err != nil {
			t.Fatalf("version %d has to be accepted: %v", v, err)
		}
	}
	for _, v := range []uint8{16, 100, 0xfe} {
		if err := mk(v).CheckSanity(); err != ErrTxBadVersion {
			t.Fatalf("version %d has to be invalid, got %v", v, err)
		}
	}
}

// A transaction from a foreign peer must not have more than one valid form.
func FuzzDeserializeTx(f *testing.F) {
	tx := &Tx{Version: 1, Inputs: []TxIn{{PrevTxID: [32]byte{1}, PrevIndex: 2, Sequence: 3}},
		Outputs: []TxOut{{Value: 100, Script: []byte("s")}},
		Witness: []Witness{{Items: [][]byte{[]byte("sig")}}}}
	f.Add(tx.Serialize())
	f.Add([]byte{})
	f.Add(make([]byte, 32))

	f.Fuzz(func(t *testing.T, data []byte) {
		got, used, err := DeserializeTx(data)
		if err != nil {
			return
		}
		if used > len(data) {
			t.Fatalf("the decoder read more than it was given")
		}
		if len(got.Inputs) > MaxTxInputs || len(got.Outputs) > MaxTxOutputs {
			t.Fatalf("accepted over the limit: %d inputs, %d outputs", len(got.Inputs), len(got.Outputs))
		}
		back := got.Serialize()
		if !reflect.DeepEqual(back, data[:used]) {
			t.Fatalf("the round-trip changed the transaction's bytes")
		}
		got.TxID()
		got.WTxID()
		_ = got.CheckSanity()
	})
}

func FuzzDeserializeBlock(f *testing.F) {
	p := RegTest
	b := blockWith(10, []*Tx{coinbaseTx(10, MinerSubsidy(10), p)})
	f.Add(b.Serialize())
	f.Add([]byte{})

	u := NewUTXOSet()
	f.Fuzz(func(t *testing.T, data []byte) {
		blk, err := DeserializeBlock(data)
		if err != nil {
			return
		}
		if len(blk.Txs) > MaxBlockTxs {
			t.Fatalf("%d transactions accepted", len(blk.Txs))
		}
		if !reflect.DeepEqual(blk.Serialize(), data) {
			t.Fatal("the round-trip changed the block's bytes")
		}
		// Validation must not crash even on a nonsensical block.
		_ = blk.ValidateBlock(u, p)
	})
}
