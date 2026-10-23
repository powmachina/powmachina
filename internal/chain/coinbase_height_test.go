package chain

import (
	"encoding/binary"
	"errors"
	"testing"
)

// A coinbase has to carry its block's height. Without it the uniqueness of its txid rests only on
// a random extranonce: two blocks with the same extranonce, the same address and the same reward
// have the same txid — and once the predecessor is fully spent such a duplicate is acceptable, so
// the transaction index has two answers to one question. In Bitcoin BIP34 solves this.
func TestCoinbaseMustCarryHeight(t *testing.T) {
	p := RegTest
	const h = 10

	// BuildCoinbase fills the height in itself, even when given an all-zero extranonce.
	var extra [32]byte
	cb := BuildCoinbase(h, nil, extra, p)
	if got := binary.LittleEndian.Uint64(cb.Inputs[0].PrevTxID[0:8]); got != h {
		t.Fatalf("BuildCoinbase did not write the height: %d", got)
	}
	// The rest of the extranonce has to stay the miner's.
	var mine [32]byte
	for i := 8; i < 32; i++ {
		mine[i] = byte(i)
	}
	cb2 := BuildCoinbase(h, nil, mine, p)
	for i := 8; i < 32; i++ {
		if cb2.Inputs[0].PrevTxID[i] != byte(i) {
			t.Fatalf("the extranonce was overwritten at byte %d", i)
		}
	}
	// And two blocks at different heights have different txids even with the same extranonce and reward.
	if BuildCoinbase(h, nil, mine, p).TxID() ==
		BuildCoinbase(h+1, nil, mine, p).TxID() {
		t.Fatal("the coinbases of two heights have the same txid")
	}

	// A block with a coinbase without the height has to be refused.
	bad := coinbaseTx(h, 0, p)
	bad.Inputs[0].PrevTxID = [32]byte{} // the height gone
	blk := blockWith(h, []*Tx{bad})
	if err := blk.ValidateBlock(NewUTXOSet(), p); !errors.Is(err, ErrCoinbaseHeight) {
		t.Fatalf("expected ErrCoinbaseHeight, got %v", err)
	}

	// And so does a coinbase with somebody else's height.
	wrong := coinbaseTx(h, 0, p)
	binary.LittleEndian.PutUint64(wrong.Inputs[0].PrevTxID[0:8], h+1)
	blk2 := blockWith(h, []*Tx{wrong})
	if err := blk2.ValidateBlock(NewUTXOSet(), p); !errors.Is(err, ErrCoinbaseHeight) {
		t.Fatalf("expected ErrCoinbaseHeight, got %v", err)
	}
}
