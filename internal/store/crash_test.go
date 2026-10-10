package store

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"powmachina/internal/chain"
)

// The crash recovery test.
//
// It is not simulated. The test **runs itself again as a subprocess**, that one writes blocks into
// the storage and the parent kills it at a random moment with **SIGKILL** — that is, with no chance
// to finish a write, close files or clean anything up. Exactly what happens on a power cut.
//
// Then the storage is opened again and the one thing that matters is verified:
// **the UTXO set matches the tip exactly.** Not "approximately", not "it can be repaired" — exactly.
// If it did not, the operator would have to resync, and that is the scenario this whole layer tries
// to prevent.

const (
	envChild = "MH_STORE_CRASH_CHILD"
	envDir   = "MH_STORE_CRASH_DIR"
)

// testChain builds a chain of blocks with coinbase transactions. Nothing is validated here —
// the storage is being tested, not consensus.
func testChain(n int) []*chain.Block {
	params := chain.RegTest
	blocks := make([]*chain.Block, 0, n)
	var prev [32]byte
	for h := 0; h < n; h++ {
		var extra [32]byte
		extra[0] = byte(h)
		extra[1] = byte(h >> 8)
		key := make([]byte, 32)
		key[0] = byte(h % 7)
		cb := chain.BuildCoinbase(uint64(h), 0, key, extra, params)
		b := &chain.Block{
			Header: &chain.Header{
				Version: 1, PrevBlock: prev, Bits: chain.RegtestTargetBits,
				Height: uint32(h), Time: uint32(1_700_000_000 + h*120), Lane: 1,
			},
			Txs: []*chain.Tx{cb},
		}
		b.Header.MerkleRoot = chain.TxMerkleRoot(b.Txs)
		prev = b.Header.ID()
		blocks = append(blocks, b)
	}
	return blocks
}

// child writes blocks until somebody kills it.
func child(dir string) {
	s, err := Open(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "child open:", err)
		os.Exit(2)
	}
	utxo := chain.NewUTXOSet()
	for i, b := range testChain(4000) {
		if err := s.PutBlock(b); err != nil {
			fmt.Fprintln(os.Stderr, "child put:", err)
			os.Exit(2)
		}
		undo, err := utxo.Apply(b)
		if err != nil {
			fmt.Fprintln(os.Stderr, "child apply:", err)
			os.Exit(2)
		}
		if err := s.ApplyBlock(b, undo); err != nil {
			fmt.Fprintln(os.Stderr, "child store:", err)
			os.Exit(2)
		}
		if i == 20 {
			// A signal to the parent that writing is really happening.
			fmt.Println("writing")
			os.Stdout.Sync()
		}
	}
	s.Close()
	os.Exit(0)
}

func TestMain(m *testing.M) {
	if dir := os.Getenv(envDir); os.Getenv(envChild) == "1" && dir != "" {
		child(dir)
		return
	}
	os.Exit(m.Run())
}

func TestSurvivesKillMinusNine(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	// Several attempts — the crash should hit different phases of a write.
	for attempt := 0; attempt < 5; attempt++ {
		dir := filepath.Join(t.TempDir(), "db")

		cmd := exec.Command(exe)
		cmd.Env = append(os.Environ(), envChild+"=1", envDir+"="+dir)
		out, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}

		// Wait until the subprocess is really writing, and then kill it.
		buf := make([]byte, 8)
		_, _ = out.Read(buf)
		time.Sleep(time.Duration(5+attempt*7) * time.Millisecond)
		if err := cmd.Process.Kill(); err != nil {
			t.Fatal(err)
		}
		_ = cmd.Wait()

		checkConsistent(t, dir, attempt)
	}
}

// checkConsistent is the core of the test: after a crash the UTXO set in the storage has to match
// exactly what comes out of replaying the blocks from the genesis up to the stored tip.
func checkConsistent(t *testing.T, dir string, attempt int) {
	t.Helper()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("attempt %d: the storage cannot be opened after the crash: %v", attempt, err)
	}
	defer s.Close()

	tip, err := s.Tip()
	if errors.Is(err, ErrNotFound) {
		t.Logf("attempt %d: killed before the first block was applied — fine", attempt)
		return
	}
	if err != nil {
		t.Fatalf("attempt %d: the tip cannot be read: %v", attempt, err)
	}

	// Replay from the genesis to the tip and compare.
	want := chain.NewUTXOSet()
	for h := uint64(0); h <= tip.Height; h++ {
		id, err := s.BlockIDAt(h)
		if err != nil {
			t.Fatalf("attempt %d: the index of height %d below the tip %d is missing: %v", attempt, h, tip.Height, err)
		}
		b, err := s.Block(id)
		if err != nil {
			t.Fatalf("attempt %d: the body of the block at height %d is missing: %v", attempt, h, err)
		}
		if _, err := want.Apply(b); err != nil {
			t.Fatalf("attempt %d: block %d cannot be replayed: %v", attempt, h, err)
		}
	}

	snap := want.Snapshot()
	got := 0
	err = s.ForEachCoin(func(op chain.Outpoint, c chain.Coin) error {
		got++
		w, ok := snap[op]
		if !ok {
			return fmt.Errorf("the storage has an extra coin %x:%d", op.TxID[:6], op.Index)
		}
		if w.Value != c.Value || w.Height != c.Height || w.Coinbase != c.Coinbase {
			return fmt.Errorf("the coin %x:%d differs: %+v vs %+v", op.TxID[:6], op.Index, w, c)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("attempt %d (tip %d): %v", attempt, tip.Height, err)
	}
	if got != len(snap) {
		t.Fatalf("attempt %d: the storage holds %d coins, the replay gave %d", attempt, got, len(snap))
	}
	t.Logf("attempt %d: after SIGKILL the tip is %d and %d coins match exactly", attempt, tip.Height, got)
}

// A restart must not mean a resync. This is the scenario the whole layer exists for: the node
// crashes, comes back up and continues where it was — without the network.
func TestRestartResumesWithoutNetwork(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "db")
	blocks := testChain(200)

	// the first run
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range blocks[:120] {
		if err := s.PutBlock(b); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ApplyBlockDirect(b); err != nil {
			t.Fatal(err)
		}
	}
	// 30 extra blocks are stored but NOT applied — exactly the state that arises from a crash
	// between storing a body and applying it.
	for _, b := range blocks[120:150] {
		if err := s.PutBlock(b); err != nil {
			t.Fatal(err)
		}
		id := b.Header.ID()
		if err := s.db.Set(keyHeight(uint64(b.Header.Height)), id[:], nil); err != nil {
			t.Fatal(err)
		}
	}
	coinsBefore, _ := s.CoinCount()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// the second run — the restore
	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("the storage cannot be opened after the restart: %v", err)
	}
	defer s2.Close()

	hc := chain.NewHeaderChain(chain.RegTest, blocks[0].Header, nil)
	height, err := s2.Restore(hc)
	if err != nil {
		t.Fatalf("the restore failed: %v", err)
	}
	if height != 119 {
		t.Fatalf("after the restore the height should be 119, it is %d", height)
	}

	applied, err := s2.ApplyStored(149, ApplyHooks{})
	if err != nil {
		t.Fatalf("applying the rest failed: %v", err)
	}
	if applied != 30 {
		t.Fatalf("30 blocks should have been applied from disk, %d were", applied)
	}
	tip, _ := s2.Tip()
	if tip.Height != 149 {
		t.Fatalf("after applying them the tip should be 149, it is %d", tip.Height)
	}
	after, _ := s2.CoinCount()
	if after <= coinsBefore {
		t.Fatalf("the coin count did not grow: %d -> %d", coinsBefore, after)
	}
	t.Logf("restart: 120 headers restored, 30 blocks applied from disk, tip %d, %d coins",
		tip.Height, after)
}
