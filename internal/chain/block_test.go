package chain

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"

	"powmachina/internal/mh"
)

// testKey builds a key pair for the tests.
func testKey(t *testing.T, seed byte) (*[32]byte, []byte) {
	t.Helper()
	sk := new([32]byte)
	for i := range sk {
		sk[i] = seed + byte(i)
	}
	pk, err := mh.SchnorrPubkey(sk)
	if err != nil {
		t.Fatal(err)
	}
	return sk, pk[:]
}

func coinbaseTx(height uint64, minerValue int64, params NetParams) *Tx {
	// The height in the coinbase is a consensus rule (BIP34 in Bitcoin) — without it the block fails.
	var extra [32]byte
	binary.LittleEndian.PutUint64(extra[0:8], height)
	tx := &Tx{
		Version: 1,
		Inputs:  []TxIn{{PrevTxID: extra, PrevIndex: CoinbaseIndex}},
	}
	if fee := DevFee(height); fee > 0 {
		tx.Outputs = append(tx.Outputs, TxOut{Value: fee, Script: params.DevFeeScript})
	}
	tx.Outputs = append(tx.Outputs, TxOut{Value: minerValue, Script: []byte("miner")})
	return tx
}

func blockWith(height uint64, txs []*Tx) *Block {
	b := &Block{
		Header: &Header{Version: 1, Height: uint32(height), Bits: RegtestTargetBits, Lane: mh.LaneC,
			Time: 1_700_000_500},
		Txs: txs,
	}
	b.Header.MerkleRoot = TxMerkleRoot(txs)
	return b
}

func TestBlockRoundTrip(t *testing.T) {
	p := RegTest
	h := uint64(10)
	cb := coinbaseTx(h, MinerSubsidy(h), p)
	_, pk := testKey(t, 3)
	spend := &Tx{Version: 1,
		Inputs:  []TxIn{{PrevTxID: [32]byte{1, 2, 3}, PrevIndex: 0, Sequence: 0xffffffff}},
		Outputs: []TxOut{{Value: 500, Script: pk}},
		Witness: []Witness{{Items: [][]byte{make([]byte, 64)}}},
	}
	b := blockWith(h, []*Tx{cb, spend})

	raw := b.Serialize()
	got, err := DeserializeBlock(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Txs) != 2 {
		t.Fatalf("expected 2 transactions, got %d", len(got.Txs))
	}
	if got.Txs[1].TxID() != spend.TxID() {
		t.Fatal("the txid changed after the round-trip")
	}
	if !bytes.Equal(got.Serialize(), raw) {
		t.Fatal("the round-trip did not give the same bytes")
	}
	// Leftover bytes must not be accepted — otherwise a block has more than one form.
	if _, err := DeserializeBlock(append(raw, 0)); err != ErrTxNotCanonical {
		t.Fatalf("leftover bytes should give ErrTxNotCanonical, they gave %v", err)
	}
}

// The witness does not enter the txid — malleability is excluded by construction.
func TestWitnessDoesNotAffectTxID(t *testing.T) {
	tx := &Tx{Version: 1,
		Inputs:  []TxIn{{PrevTxID: [32]byte{7}, PrevIndex: 1}},
		Outputs: []TxOut{{Value: 100, Script: []byte("x")}},
		Witness: []Witness{{Items: [][]byte{[]byte("signature A")}}},
	}
	id1 := tx.TxID()
	w1 := tx.WTxID()
	tx.Witness = []Witness{{Items: [][]byte{[]byte("signature B, different")}}}
	if tx.TxID() != id1 {
		t.Fatal("changing the witness changed the txid")
	}
	if tx.WTxID() == w1 {
		t.Fatal("changing the witness has to change the wtxid")
	}
}

func TestCoinbaseRules(t *testing.T) {
	p := RegTest
	u := NewUTXOSet()
	h := uint64(10)

	// A correct block.
	ok := blockWith(h, []*Tx{coinbaseTx(h, MinerSubsidy(h), p)})
	if err := ok.ValidateBlock(u, p); err != nil {
		t.Fatalf("a valid block was refused: %v", err)
	}

	// The miner took more than they may.
	greedy := blockWith(h, []*Tx{coinbaseTx(h, Subsidy(h), p)})
	if err := greedy.ValidateBlock(u, p); err == nil {
		t.Fatal("a coinbase over the limit passed")
	}

	// The dev fee is missing.
	noFee := &Tx{Version: 1, Inputs: []TxIn{{PrevIndex: CoinbaseIndex}},
		Outputs: []TxOut{{Value: MinerSubsidy(h), Script: []byte("miner")}}}
	if err := blockWith(h, []*Tx{noFee}).ValidateBlock(u, p); err == nil {
		t.Fatal("a block without the dev fee output passed")
	}

	// The dev fee is one cog short.
	short := coinbaseTx(h, MinerSubsidy(h), p)
	short.Outputs[0].Value--
	if err := blockWith(h, []*Tx{short}).ValidateBlock(u, p); err == nil {
		t.Fatal("a short dev fee passed")
	}

	// The dev fee is not the first output.
	swapped := coinbaseTx(h, MinerSubsidy(h), p)
	swapped.Outputs[0], swapped.Outputs[1] = swapped.Outputs[1], swapped.Outputs[0]
	if err := blockWith(h, []*Tx{swapped}).ValidateBlock(u, p); err == nil {
		t.Fatal("a dev fee outside the first output passed")
	}

	// After the dev fee ends it is no longer required.
	late := uint64(DevFeeEndHeight + 1)
	lateBlock := blockWith(late, []*Tx{coinbaseTx(late, Subsidy(late), p)})
	if err := lateBlock.ValidateBlock(u, p); err != nil {
		t.Fatalf("after the dev fee ends the block should pass: %v", err)
	}

	// Two coinbases in one block.
	two := blockWith(h, []*Tx{coinbaseTx(h, MinerSubsidy(h), p), coinbaseTx(h, MinerSubsidy(h), p)})
	if err := two.ValidateBlock(u, p); !errors.Is(err, ErrExtraCoinbase) && !errors.Is(err, ErrDuplicateTx) {
		t.Fatalf("two coinbases should be refused, got %v", err)
	}
}

func TestSpendingRules(t *testing.T) {
	p := RegTest
	u := NewUTXOSet()
	sk, pk := testKey(t, 11)
	prev := Outpoint{TxID: [32]byte{42}, Index: 0}
	prevCoin := Coin{Value: 1000, Script: pk, Height: 5}
	u.AddCoin(prev, prevCoin)

	h := uint64(10)
	// mk assembles a block and signs the inputs with the key sk right away.
	mk := func(in []TxIn, out []TxOut) *Block {
		tx := &Tx{Version: 1, Inputs: in, Outputs: out}
		coins := make([]Coin, len(in))
		for i := range in {
			if c, ok := u.Get(Outpoint{TxID: in[i].PrevTxID, Index: in[i].PrevIndex}); ok {
				coins[i] = c
			}
		}
		tx.Witness = make([]Witness, len(in))
		for i := range in {
			sig, err := SignInput(tx, i, coins, sk)
			if err != nil {
				t.Fatal(err)
			}
			tx.Witness[i] = Witness{Items: [][]byte{sig}}
		}
		return blockWith(h, []*Tx{coinbaseTx(h, MinerSubsidy(h), p), tx})
	}

	// A fee of 100 cogs.
	good := mk([]TxIn{{PrevTxID: prev.TxID, PrevIndex: 0}}, []TxOut{{Value: 900, Script: pk}})
	if err := good.ValidateBlock(u, p); err != nil {
		t.Fatalf("a valid transaction was refused: %v", err)
	}

	// Outputs above the inputs.
	over := mk([]TxIn{{PrevTxID: prev.TxID, PrevIndex: 0}}, []TxOut{{Value: 1001, Script: pk}})
	if err := over.ValidateBlock(u, p); err == nil {
		t.Fatal("a transaction creating money passed")
	}

	// A non-existent input.
	ghost := mk([]TxIn{{PrevTxID: [32]byte{99}, PrevIndex: 0}}, []TxOut{{Value: 1, Script: pk}})
	if err := ghost.ValidateBlock(u, p); err == nil {
		t.Fatal("a non-existent input passed")
	}

	// The same input twice in one transaction.
	twice := mk([]TxIn{{PrevTxID: prev.TxID, PrevIndex: 0}, {PrevTxID: prev.TxID, PrevIndex: 0}},
		[]TxOut{{Value: 1, Script: pk}})
	if err := twice.ValidateBlock(u, p); err == nil {
		t.Fatal("a double spend inside a transaction passed")
	}

	// An immature coinbase.
	cbOut := Outpoint{TxID: [32]byte{77}, Index: 0}
	u.AddCoin(cbOut, Coin{Value: 500, Script: pk, Height: h - 1, Coinbase: true})
	immature := mk([]TxIn{{PrevTxID: cbOut.TxID, PrevIndex: 0}}, []TxOut{{Value: 1, Script: pk}})
	if err := immature.ValidateBlock(u, p); !errors.Is(err, ErrCoinImmature) {
		t.Fatalf("an immature coinbase should give ErrCoinImmature, got %v", err)
	}
}

func TestMerkleProperties(t *testing.T) {
	id := func(b byte) [32]byte { var x [32]byte; x[0] = b; return x }

	// The order of the transactions has to be part of the root.
	if MerkleRoot([][32]byte{id(1), id(2)}) == MerkleRoot([][32]byte{id(2), id(1)}) {
		t.Fatal("the root does not depend on the order")
	}
	// An odd count must not be handled by duplicating (CVE-2012-2459).
	three := MerkleRoot([][32]byte{id(1), id(2), id(3)})
	four := MerkleRoot([][32]byte{id(1), id(2), id(3), id(3)})
	if three == four {
		t.Fatal("a duplicated last transaction gives the same root — that is CVE-2012-2459")
	}
	// A leaf and an internal node have different domains.
	single := MerkleRoot([][32]byte{id(1)})
	if single == id(1) {
		t.Fatal("a leaf has to be hashed, not taken over directly")
	}
	if MerkleRoot(nil) != [32]byte{} {
		t.Fatal("an empty list has to give a zero root")
	}
}

// A block with the wrong merkle root must not be accepted.
func TestBadMerkleRejected(t *testing.T) {
	p := RegTest
	h := uint64(10)
	b := blockWith(h, []*Tx{coinbaseTx(h, MinerSubsidy(h), p)})
	b.Header.MerkleRoot[0] ^= 0xff
	if err := b.ValidateBlock(NewUTXOSet(), p); err != ErrBadMerkle {
		t.Fatalf("expected ErrBadMerkle, got %v", err)
	}
}
