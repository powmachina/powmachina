package store

import (
	"path/filepath"
	"testing"

	"powmachina/internal/chain"
)

// chainOnGenesis builds blocks following the regtest genesis. Nothing is validated —
// the storage is being tested, not consensus.
func chainOnGenesis(n int) []*chain.Block {
	params := chain.RegTest
	g := chain.RegtestGenesis()
	prev := g.ID()
	blocks := make([]*chain.Block, 0, n)
	for i := range n {
		h := uint64(i + 1)
		var extra [32]byte
		extra[0] = byte(h)
		key := make([]byte, 32)
		key[0] = byte(h % 7)
		b := &chain.Block{
			Header: &chain.Header{
				Version: 1, PrevBlock: prev, Bits: chain.RegtestTargetBits,
				Height: uint32(h), Time: g.Time + uint32(h)*120, Lane: 1,
			},
			Txs: []*chain.Tx{chain.BuildCoinbase(h, 0, key, extra, params)},
		}
		b.Header.MerkleRoot = chain.TxMerkleRoot(b.Txs)
		prev = b.Header.ID()
		blocks = append(blocks, b)
	}
	return blocks
}

// A node that mines blocks or receives them whole never goes through PutHeader — it goes through
// PutBlock + ApplyBlockDirect. If that path did not move the verified-headers mark, it would stay
// at the genesis and Restore would report height 0 after a restart: the node would throw away a
// chain it has in full on disk and start syncing from scratch.
//
// Only a restart shows it — hence a node that mines 25 blocks and comes back up.
func TestRestoreAfterMiningPath(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "chain")
	g := chain.RegtestGenesis()
	blocks := chainOnGenesis(25)

	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.InitGenesis(g); err != nil {
		t.Fatal(err)
	}
	for _, b := range blocks {
		if err := s.PutBlock(b); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ApplyBlockDirect(b); err != nil {
			t.Fatalf("block %d: %v", b.Header.Height, err)
		}
	}
	// The header mark must not lag behind the applied tip — that is the invariant.
	ht, err := s.HeaderTip()
	if err != nil {
		t.Fatalf("headerTip: %v", err)
	}
	tip, err := s.Tip()
	if err != nil {
		t.Fatalf("tip: %v", err)
	}
	if ht.Height < tip.Height {
		t.Fatalf("headerTip is at height %d, the tip at %d — Restore turns that into a resync",
			ht.Height, tip.Height)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	hc := chain.NewHeaderChain(chain.RegTest, g, nil)
	height, err := s2.Restore(hc)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if height != 25 {
		t.Fatalf("the restore ended at height %d, there are 25 on disk", height)
	}
	if _, h, _ := hc.Tip(); h != 25 {
		t.Fatalf("HeaderChain is at height %d, the storage at 25 — the node would run with two different tips", h)
	}
}

// A datadir with the header mark at the genesis and the applied tip high up. Restore has to
// recover from it rather than throw it away — otherwise such a node would resync from scratch.
func TestRestoreRecoversStaleHeaderTip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "chain")
	g := chain.RegtestGenesis()
	blocks := chainOnGenesis(10)

	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.InitGenesis(g); err != nil {
		t.Fatal(err)
	}
	for _, b := range blocks {
		if err := s.PutBlock(b); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ApplyBlockDirect(b); err != nil {
			t.Fatal(err)
		}
	}
	// Put the header mark back at the genesis.
	gid := g.ID()
	if err := s.db.Set(keyHeaderTip, encodeTip(Tip{ID: gid, Height: 0}), nil); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	height, err := s2.Restore(chain.NewHeaderChain(chain.RegTest, g, nil))
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if height != 10 {
		t.Fatalf("the restore ended at height %d, there are 10 on disk", height)
	}
}
