package store

import (
	"testing"

	"powmachina/internal/chain"
)

// "No such coin" and "the coin could not be read" are different answers: the first makes a block that
// spends it invalid for ever, the second says nothing about the block.
func TestLookupCoinTellsMissingFromUnreadable(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	op := chain.Outpoint{TxID: [32]byte{7}, Index: 1}
	if _, ok, err := s.LookupCoin(op); ok || err != nil {
		t.Fatalf("a coin nobody stored: found %v, err %v", ok, err)
	}
	if err := s.AddCoin(op, chain.Coin{Value: 5, Script: make([]byte, 32)}); err != nil {
		t.Fatal(err)
	}
	if c, ok, err := s.LookupCoin(op); !ok || err != nil || c.Value != 5 {
		t.Fatalf("a stored coin: found %v, err %v", ok, err)
	}

	// What is on disk under the key is not a coin.
	if err := s.db.Set(keyCoin(op.TxID, op.Index), []byte{0xff}, nil); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.LookupCoin(op); ok || err == nil {
		t.Fatalf("a corrupted coin: found %v, err %v — it must not read as missing", ok, err)
	}
	if _, ok := s.Coin(op); ok {
		t.Fatal("Coin reported a corrupted coin as found")
	}
}
