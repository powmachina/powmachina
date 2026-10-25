package store

import (
	"bytes"
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	"powmachina/internal/chain"
)

func testCoin(v int64, h uint64, cb bool, script string) chain.Coin {
	return chain.Coin{
		Value: v, ScriptVersion: 0, Script: []byte(script), Height: h, Coinbase: cb,
	}
}

func TestCoinRoundTrip(t *testing.T) {
	for _, c := range []chain.Coin{
		testCoin(0, 0, false, ""),
		testCoin(1, 1, true, "a"),
		testCoin(-1, 1<<40, false, string(make([]byte, 520))),
		testCoin(2_100_000_000_000_000, 999, true, "\x00\xff\x00"),
	} {
		b := encodeCoin(c)
		got, err := decodeCoin(b)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got.Value != c.Value || got.ScriptVersion != c.ScriptVersion ||
			got.Height != c.Height || got.Coinbase != c.Coinbase ||
			!bytes.Equal(got.Script, c.Script) {
			t.Fatalf("the round trip changed the coin:\n %+v\n %+v", c, got)
		}
	}
}

// A truncated coin has to be refused, not silently read as a different value.
func TestCoinRejectsTruncated(t *testing.T) {
	full := encodeCoin(testCoin(42, 7, false, "abcd"))
	for n := 0; n < len(full); n++ {
		if _, err := decodeCoin(full[:n]); err == nil {
			t.Fatalf("a coin truncated to %d B passed", n)
		}
	}
}

func TestUndoRoundTrip(t *testing.T) {
	u := &chain.BlockUndo{Height: 12345}
	for i := 0; i < 3; i++ {
		var op chain.Outpoint
		op.TxID[0] = byte(i + 1)
		op.Index = uint32(i)
		u.Spent = append(u.Spent, chain.UndoEntry{Outpoint: op, Coin: testCoin(int64(i*100), uint64(i), i == 0, "s")})
		var cr chain.Outpoint
		cr.TxID[31] = byte(i + 9)
		cr.Index = uint32(i * 2)
		u.Created = append(u.Created, cr)
	}
	got, err := decodeUndo(encodeUndo(u))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, u) {
		t.Fatalf("the round trip changed the undo:\n %+v\n %+v", u, got)
	}
}

func TestSchemaVersionIsChecked(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Plant a different version and verify that the directory refuses to open.
	if err := s.db.Set(keyVersion, []byte{0, 0, 0, 99}, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir); !errors.Is(err, ErrWrongSchema) {
		t.Fatalf("a different schema version has to be refused, got %v", err)
	}
}

func TestOpenEmptyHasNoTip(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.Tip(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("empty storage should have no tip, got %v", err)
	}
}

// The disk path has to give exactly the same thing as the in-memory one — otherwise the node and the miner would see different state.
func TestDirectApplyMatchesMemory(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	mem := chain.NewUTXOSet()
	for _, b := range testChain(50) {
		if err := s.PutBlock(b); err != nil {
			t.Fatal(err)
		}
		wantUndo, err := mem.Apply(b)
		if err != nil {
			t.Fatal(err)
		}
		gotUndo, err := s.ApplyBlockDirect(b)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(gotUndo, wantUndo) {
			t.Fatalf("height %d: the undo data differs\n disk: %+v\n memory: %+v", b.Header.Height, gotUndo, wantUndo)
		}
	}

	snap := mem.Snapshot()
	n, err := s.CoinCount()
	if err != nil {
		t.Fatal(err)
	}
	if n != len(snap) {
		t.Fatalf("%d coins on disk, %d in memory", n, len(snap))
	}
	for op, want := range snap {
		got, ok := s.Coin(op)
		if !ok {
			t.Fatalf("the coin %x:%d is missing on disk", op.TxID[:6], op.Index)
		}
		if got.Value != want.Value || got.Height != want.Height || got.Coinbase != want.Coinbase {
			t.Fatalf("the coin %x:%d differs: %+v vs %+v", op.TxID[:6], op.Index, got, want)
		}
	}
}

// A revert has to restore exactly the previous state — a reorg stands on that.
func TestRevertRestoresPreviousState(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	blocks := testChain(10)
	for _, b := range blocks[:9] {
		if err := s.PutBlock(b); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ApplyBlockDirect(b); err != nil {
			t.Fatal(err)
		}
	}
	before, err := s.CoinCount()
	if err != nil {
		t.Fatal(err)
	}
	tipBefore, _ := s.Tip()

	last := blocks[9]
	if err := s.PutBlock(last); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyBlockDirect(last); err != nil {
		t.Fatal(err)
	}
	if err := s.RevertTip(); err != nil {
		t.Fatal(err)
	}

	after, err := s.CoinCount()
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("after the revert there are %d coins, before the apply there were %d", after, before)
	}
	tipAfter, err := s.Tip()
	if err != nil {
		t.Fatal(err)
	}
	if tipAfter != tipBefore {
		t.Fatalf("the tip did not come back: %+v vs %+v", tipAfter, tipBefore)
	}
}

// A block that does not build on the tip has to be refused — otherwise it would silently break the UTXO set.
func TestApplyRejectsGap(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	blocks := testChain(5)
	if _, err := s.ApplyBlockDirect(blocks[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyBlockDirect(blocks[2]); !errors.Is(err, ErrTipMismatch) {
		t.Fatalf("a skipped block has to be refused, got %v", err)
	}
}

// The disk's unwind of a block in which one transaction spends another's output (PM-01): the
// coins have to be exactly those before the block, the in-between output included.
func TestRevertOfAChainInsideOneBlock(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	blocks := testChain(10)
	for _, b := range blocks[:9] {
		if err := s.PutBlock(b); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ApplyBlockDirect(b); err != nil {
			t.Fatal(err)
		}
	}
	// The extra output testChain puts into block 0's coinbase: A spends it, B spends A's output.
	cb0 := blocks[0].Txs[0]
	u := chain.Outpoint{TxID: cb0.TxID(), Index: uint32(len(cb0.Outputs) - 1)}
	if _, ok := s.Coin(u); !ok {
		t.Fatal("the coin to spend is not there")
	}
	a := &chain.Tx{Version: 1, Inputs: []chain.TxIn{{PrevTxID: u.TxID, PrevIndex: u.Index}},
		Outputs: []chain.TxOut{{Value: 1, Script: []byte("x")}}}
	x := chain.Outpoint{TxID: a.TxID(), Index: 0}
	b := &chain.Tx{Version: 1, Inputs: []chain.TxIn{{PrevTxID: x.TxID, PrevIndex: 0}},
		Outputs: []chain.TxOut{{Value: 1, Script: []byte("y")}}}
	last := blocks[9]
	last.Txs = append(last.Txs, a, b)
	last.Seal()

	before, err := s.CoinCount()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PutBlock(last); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyBlockDirect(last); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Coin(x); ok {
		t.Fatal("the output spent within the block is still there")
	}
	if err := s.RevertTip(); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Coin(x); ok {
		t.Fatal("the revert brought back an output that did not exist before the block")
	}
	if _, ok := s.Coin(u); !ok {
		t.Fatal("the revert did not bring back the coin the block spent")
	}
	if after, err := s.CoinCount(); err != nil || after != before {
		t.Fatalf("after the revert %d coins (%v), before the block %d", after, err, before)
	}
}
