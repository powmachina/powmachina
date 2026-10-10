package store

import (
	"errors"
	"testing"

	"powmachina/internal/chain"
)

// spendChain builds a chain in which the coinbase of block 0 is spent in block 1:
// address A receives, then sends to B and returns the rest as change.
//
// It is the smallest graph both things can be verified on — a receive and a spend — and at the same
// time whether the change (going back to A) is counted correctly.
func spendChain(t *testing.T) (blocks []*chain.Block, keyA, keyB [32]byte) {
	t.Helper()
	keyA[0], keyB[0] = 0xAA, 0xBB

	cb := &chain.Tx{
		Version: 1,
		Inputs:  []chain.TxIn{{PrevIndex: chain.CoinbaseIndex}},
		Outputs: []chain.TxOut{{Value: 1_000_000_000, Script: keyA[:]}},
	}
	genesis := &chain.Block{
		Header: &chain.Header{Version: 1, Bits: chain.RegtestTargetBits, Height: 0,
			Time: 1_700_000_000, Lane: 1},
		Txs: []*chain.Tx{cb},
	}
	genesis.Header.MerkleRoot = chain.TxMerkleRoot(genesis.Txs)

	cb2 := &chain.Tx{
		Version: 1,
		Inputs:  []chain.TxIn{{PrevTxID: [32]byte{1}, PrevIndex: chain.CoinbaseIndex}},
		Outputs: []chain.TxOut{{Value: 500_000_000, Script: keyB[:]}},
	}
	// A spends its coinbase: 300 M goes to B, 690 M back to A, 10 M is the fee.
	cbID := cb.TxID()
	spend := &chain.Tx{
		Version: 1,
		Inputs:  []chain.TxIn{{PrevTxID: cbID, PrevIndex: 0}},
		Outputs: []chain.TxOut{
			{Value: 300_000_000, Script: keyB[:]},
			{Value: 690_000_000, Script: keyA[:]},
		},
	}
	second := &chain.Block{
		Header: &chain.Header{Version: 1, PrevBlock: genesis.Header.ID(),
			Bits: chain.RegtestTargetBits, Height: 1, Time: 1_700_000_120, Lane: 1},
		Txs: []*chain.Tx{cb2, spend},
	}
	second.Header.MerkleRoot = chain.TxMerkleRoot(second.Txs)
	return []*chain.Block{genesis, second}, keyA, keyB
}

func openIndexed(t *testing.T) *Store {
	t.Helper()
	s, err := OpenMemWith(Options{TxIndex: true, AddrIndex: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func applyAll(t *testing.T, s *Store, blocks []*chain.Block) {
	t.Helper()
	for _, b := range blocks {
		if err := s.PutBlock(b); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ApplyBlockDirect(b); err != nil {
			t.Fatalf("height %d: %v", b.Header.Height, err)
		}
	}
}

func TestAddrIndexOffIsDistinguishable(t *testing.T) {
	s, err := OpenMem()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.HasAddrIndex() {
		t.Fatal("the index should not have been enabled")
	}
	if _, err := s.AddrSummary(ScriptKey{}); !errors.Is(err, ErrNoAddrIndex) {
		t.Fatalf("expected ErrNoAddrIndex, got %v", err)
	}
}

func TestAddrSummaryTracksReceivedAndSent(t *testing.T) {
	s := openIndexed(t)
	blocks, keyA, keyB := spendChain(t)
	applyAll(t, s, blocks)

	skA := ScriptKeyFor(0, keyA[:])
	skB := ScriptKeyFor(0, keyB[:])

	a, err := s.AddrSummary(skA)
	if err != nil {
		t.Fatal(err)
	}
	// A received 1 000 M (the coinbase) + 690 M (the change) and spent 1 000 M.
	if a.Received != 1_690_000_000 || a.Sent != 1_000_000_000 {
		t.Fatalf("A: received %d, spent %d", a.Received, a.Sent)
	}
	if a.Balance() != 690_000_000 {
		t.Fatalf("A has a balance of %d, expected 690000000", a.Balance())
	}
	// Two transactions touched it: the coinbase in block 0 and the spend in block 1.
	if a.TxCount != 2 {
		t.Fatalf("%d transactions touched A, expected 2", a.TxCount)
	}

	b, err := s.AddrSummary(skB)
	if err != nil {
		t.Fatal(err)
	}
	if b.Received != 800_000_000 || b.Sent != 0 || b.Balance() != 800_000_000 {
		t.Fatalf("B: received %d, spent %d, balance %d", b.Received, b.Sent, b.Balance())
	}

	// The summary keeps the script, so an address can be built from it without looking the output up.
	if a.ScriptVersion != 0 || len(a.Script) != 32 {
		t.Fatalf("the summary does not keep the script: version %d, %d B", a.ScriptVersion, len(a.Script))
	}
}

// An unknown address is a valid address with a zero balance, not an error.
func TestAddrSummaryUnknownIsZero(t *testing.T) {
	s := openIndexed(t)
	var never [32]byte
	never[0] = 0xEE
	sum, err := s.AddrSummary(ScriptKeyFor(0, never[:]))
	if err != nil {
		t.Fatalf("an unknown address must not be an error: %v", err)
	}
	if sum.Balance() != 0 || sum.TxCount != 0 {
		t.Fatalf("an unknown address has a balance of %d and %d tx", sum.Balance(), sum.TxCount)
	}
}

func TestAddrHistoryHasBothDirections(t *testing.T) {
	s := openIndexed(t)
	blocks, keyA, _ := spendChain(t)
	applyAll(t, s, blocks)

	hist, err := s.AddrHistory(ScriptKeyFor(0, keyA[:]), 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	// the coinbase (a receive, h. 0), the spend (h. 1) and the change (a receive, h. 1)
	if len(hist) != 3 {
		t.Fatalf("expected 3 movements, got %d", len(hist))
	}
	// The newest first.
	if hist[0].Height != 1 || hist[len(hist)-1].Height != 0 {
		t.Fatalf("the history is not from the newest: %d … %d",
			hist[0].Height, hist[len(hist)-1].Height)
	}
	var received, sent int64
	for _, e := range hist {
		if e.Received {
			received += e.Value
		} else {
			sent += e.Value
		}
	}
	if received != 1_690_000_000 || sent != 1_000_000_000 {
		t.Fatalf("the history gives received %d, spent %d", received, sent)
	}
}

// This is the test the index lives in the node's storage for, rather than in a separate database.
//
// An earlier explorer spent time on this: an unwound block left the balances shifted ("a transaction got
// deducted double", negative balances). Here the index is unwound in the same batch as the block, so
// after a revert the state has to be **exactly** the one from before.
func TestAddrIndexUnwindsWithReorg(t *testing.T) {
	s := openIndexed(t)
	blocks, keyA, keyB := spendChain(t)
	skA, skB := ScriptKeyFor(0, keyA[:]), ScriptKeyFor(0, keyB[:])

	applyAll(t, s, blocks[:1])
	beforeA, _ := s.AddrSummary(skA)
	beforeB, _ := s.AddrSummary(skB)
	histBefore, _ := s.AddrHistory(skA, 0, 50)

	applyAll(t, s, blocks[1:])
	if err := s.RevertTip(); err != nil {
		t.Fatal(err)
	}

	afterA, _ := s.AddrSummary(skA)
	afterB, _ := s.AddrSummary(skB)
	if afterA.Received != beforeA.Received || afterA.Sent != beforeA.Sent ||
		afterA.TxCount != beforeA.TxCount {
		t.Fatalf("A did not come back: %+v vs %+v", afterA, beforeA)
	}
	if afterB.Balance() != beforeB.Balance() || afterB.TxCount != beforeB.TxCount {
		t.Fatalf("B did not come back: balance %d vs %d", afterB.Balance(), beforeB.Balance())
	}
	if afterB.Balance() != 0 {
		t.Fatalf("B never received anything before block 1, it has %d", afterB.Balance())
	}

	histAfter, _ := s.AddrHistory(skA, 0, 50)
	if len(histAfter) != len(histBefore) {
		t.Fatalf("A's history has %d movements after the revert, before the block it had %d",
			len(histAfter), len(histBefore))
	}
	for i := range histAfter {
		if histAfter[i] != histBefore[i] {
			t.Fatalf("movement %d changed: %+v vs %+v", i, histAfter[i], histBefore[i])
		}
	}

	// And after applying it again the state has to be the later one once more.
	applyAll(t, s, blocks[1:])
	againA, _ := s.AddrSummary(skA)
	if againA.Balance() != 690_000_000 {
		t.Fatalf("after applying again A has %d, expected 690000000", againA.Balance())
	}
}

// Turning the address index on over an existing datadir means walking the chain from scratch —
// and exactly the same thing has to come out as if the index had run from the beginning.
func TestReindexAddrsMatchesIncremental(t *testing.T) {
	blocks, keyA, keyB := spendChain(t)
	skA, skB := ScriptKeyFor(0, keyA[:]), ScriptKeyFor(0, keyB[:])

	// The reference: the index on from the start.
	ref := openIndexed(t)
	applyAll(t, ref, blocks)
	wantA, _ := ref.AddrSummary(skA)
	wantB, _ := ref.AddrSummary(skB)
	wantHist, _ := ref.AddrHistory(skA, 0, 50)

	// A datadir without the index, then turned on and reindexed.
	plain, err := OpenMem()
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Close()
	applyAll(t, plain, blocks)

	plain.indexAddr = true // as if it restarted with -addrindex
	if plain.AddrIndexReady() {
		t.Fatal("the index marked itself ready without a reindex")
	}
	if err := plain.ReindexAddrs(nil); err != nil {
		t.Fatal(err)
	}
	if !plain.AddrIndexReady() {
		t.Fatal("the readiness mark is missing after the reindex")
	}

	gotA, _ := plain.AddrSummary(skA)
	gotB, _ := plain.AddrSummary(skB)
	if gotA.Received != wantA.Received || gotA.Sent != wantA.Sent || gotA.TxCount != wantA.TxCount {
		t.Fatalf("A after the reindex %+v, incrementally %+v", gotA, wantA)
	}
	if gotB.Balance() != wantB.Balance() {
		t.Fatalf("B after the reindex %d, incrementally %d", gotB.Balance(), wantB.Balance())
	}
	gotHist, _ := plain.AddrHistory(skA, 0, 50)
	if len(gotHist) != len(wantHist) {
		t.Fatalf("the history has %d movements, incrementally %d", len(gotHist), len(wantHist))
	}
	for i := range gotHist {
		if gotHist[i] != wantHist[i] {
			t.Fatalf("movement %d: %+v vs %+v", i, gotHist[i], wantHist[i])
		}
	}
}

// The rich list has to be ordered descending and must not contain an address twice — the old key is
// deleted when the balance changes. Otherwise the same address would appear with two balances and the
// sum of the shares would exceed 100 % (iquidus: "Richlist pie chart sums to 126%").
func TestRichListIsSortedAndUnique(t *testing.T) {
	s := openIndexed(t)
	blocks, keyA, keyB := spendChain(t)
	applyAll(t, s, blocks)

	list, err := s.RichList(50)
	if err != nil {
		t.Fatal(err)
	}
	// A has 690 M, B has 800 M → B has to be first.
	if len(list) != 2 {
		t.Fatalf("expected 2 addresses, got %d: %+v", len(list), list)
	}
	if list[0].Balance != 800_000_000 || list[1].Balance != 690_000_000 {
		t.Fatalf("the wrong order: %d, %d", list[0].Balance, list[1].Balance)
	}
	if list[0].Key != ScriptKeyFor(0, keyB[:]) || list[1].Key != ScriptKeyFor(0, keyA[:]) {
		t.Fatal("the list holds different addresses than expected")
	}
	// A row has to carry the script, so an address can be built from it.
	if len(list[0].Script) != 32 {
		t.Fatalf("the row does not keep the script (%d B)", len(list[0].Script))
	}

	// No address twice.
	seen := map[ScriptKey]int{}
	for _, e := range list {
		seen[e.Key]++
		if seen[e.Key] > 1 {
			t.Fatalf("the address %x is in the list %d times — the old key was not deleted",
				e.Key[:6], seen[e.Key])
		}
	}

	// The sum of the balances in the list has to match what the summaries say.
	var total int64
	for _, e := range list {
		sum, err := s.AddrSummary(e.Key)
		if err != nil {
			t.Fatal(err)
		}
		if sum.Balance() != e.Balance {
			t.Fatalf("the rich list reports %d, the summary %d", e.Balance, sum.Balance())
		}
		total += e.Balance
	}
	if total != 1_490_000_000 {
		t.Fatalf("the sum of the balances is %d, expected 1490000000", total)
	}
}

// After an unwind the rich list has to look exactly as it did before the block — including the
// addresses that were not in it then.
func TestRichListUnwindsWithReorg(t *testing.T) {
	s := openIndexed(t)
	blocks, _, keyB := spendChain(t)

	applyAll(t, s, blocks[:1])
	before, _ := s.RichList(50)

	applyAll(t, s, blocks[1:])
	if err := s.RevertTip(); err != nil {
		t.Fatal(err)
	}
	after, err := s.RichList(50)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("after the revert the list holds %d addresses, before the block it held %d", len(after), len(before))
	}
	for i := range after {
		if after[i].Key != before[i].Key || after[i].Balance != before[i].Balance {
			t.Fatalf("row %d: %x/%d vs %x/%d", i,
				after[i].Key[:6], after[i].Balance, before[i].Key[:6], before[i].Balance)
		}
	}
	// B did not exist before block 1, so it must not be in the list.
	skB := ScriptKeyFor(0, keyB[:])
	for _, e := range after {
		if e.Key == skB {
			t.Fatal("B is in the list even though it received nothing before block 1")
		}
	}
}
