package chain

import (
	"encoding/hex"
	"errors"
	"math/big"
	"strings"
	"testing"
)

// A checkpoint has to apply to a restore from disk as well. Otherwise a node that once downloaded
// a fake branch would come up on it again after a restart and a new rule in the binary would mean nothing.
func TestCheckpointAppliesOnLoad(t *testing.T) {
	p := RegTest
	g := RegtestGenesis()
	hc := NewHeaderChain(p, g, nil)

	h1 := &Header{Version: 1, PrevBlock: g.ID(), Height: 1, Bits: RegtestTargetBits,
		Lane: g.Lane, Time: g.Time + 1}
	if err := hc.LoadHeader(h1); err != nil {
		t.Fatalf("without a checkpoint the header should have passed: %v", err)
	}

	// The same height, but the checkpoint says a different block.
	p2 := RegTest
	p2.Checkpoints = []Checkpoint{{Height: 1, ID: [32]byte{0xCC}}}
	hc2 := NewHeaderChain(p2, g, nil)
	if err := hc2.LoadHeader(h1); !errors.Is(err, ErrCheckpoint) {
		t.Fatalf("expected ErrCheckpoint on a restore, got %v", err)
	}
	// And the same from the network.
	if _, err := hc2.AddHeader(h1, nil); !errors.Is(err, ErrCheckpoint) {
		t.Fatalf("expected ErrCheckpoint from the network, got %v", err)
	}
}

// MinChainWork is not a consensus rule — headers keep being accepted — but a chain below it must
// not report itself as trustworthy. That is the signal for an exchange and a wallet.
func TestTrustedWorkFollowsMinChainWork(t *testing.T) {
	g := RegtestGenesis()

	// A network without the value: always trustworthy.
	if !NewHeaderChain(RegTest, g, nil).TrustedWork() {
		t.Fatal("without MinChainWork the chain should be trustworthy")
	}

	p := RegTest
	p.MinChainWork = "ffffffffffffffff" // unreachable with one block
	hc := NewHeaderChain(p, g, nil)
	if hc.TrustedWork() {
		t.Fatal("a chain below MinChainWork must not report itself as trustworthy")
	}
	// The headers still have to be accepted, otherwise the node has no way to sync.
	h1 := &Header{Version: 1, PrevBlock: g.ID(), Height: 1, Bits: RegtestTargetBits,
		Lane: g.Lane, Time: g.Time + 1}
	if err := hc.LoadHeader(h1); err != nil {
		t.Fatalf("the header should have been accepted even below MinChainWork: %v", err)
	}

	// A low bar: met.
	p.MinChainWork = "01"
	hc2 := NewHeaderChain(p, g, nil)
	if err := hc2.LoadHeader(h1); err != nil {
		t.Fatal(err)
	}
	if !hc2.TrustedWork() {
		t.Fatalf("a chain with work %v above a bar of 1 should be trustworthy", hc2.Work())
	}
	// A corrupted value is ignored rather than panicking.
	p.MinChainWork = "this is not hex"
	if w := p.MinWork(); w != nil {
		t.Fatalf("a corrupted value should have given nil, it gave %v", w)
	}
	_ = big.NewInt(0)
}

// The testnet's baked-in values have to be parseable and have to match the real network.
// A typo in the hex would otherwise only show up as the node refusing a block that is perfectly
// fine — and that looks like a network fault, not like a bug in a literal.
func TestTestnetCheckpointsAreWellFormed(t *testing.T) {
	// A network that has only just started has nothing to pin yet, so the values may be empty.
	// So the test does not check that they are there, but that whatever is there makes sense — an
	// empty list passes, a typo in the hex does not.
	if TestNet.MinChainWork != "" {
		if w := TestNet.MinWork(); w == nil || w.Sign() <= 0 {
			t.Fatalf("MinChainWork %q cannot be read as a number", TestNet.MinChainWork)
		}
	}
	for _, cp := range TestNet.Checkpoints {
		if cp.Height == 0 {
			t.Error("a checkpoint at height 0 makes no sense — the genesis is in the binary")
		}
		if cp.ID == ([32]byte{}) {
			t.Errorf("the checkpoint at height %d has an empty ID", cp.Height)
		}
		// A checkpoint has to be **below** the finalized depth, otherwise a block that can still fall
		// out of the active branch could be sealed in.
		got, ok := TestNet.checkpointAt(cp.Height)
		if !ok || got != cp.ID {
			t.Errorf("checkpointAt(%d) does not return the baked-in ID", cp.Height)
		}
	}
}

// mustID is the only place a hex literal turns into an ID — bad input has to stop startup rather
// than pass as a zero ID.
func TestMustIDRejectsGarbage(t *testing.T) {
	for _, bad := range []string{"", "ab", strings.Repeat("z", 64), strings.Repeat("0", 63), strings.Repeat("0", 66)} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("mustID(%q) did not panic", bad)
				}
			}()
			mustID(bad)
		}()
	}
	id := mustID("a3010fa5658e7e318cf00127f922bcb26c33c3bc211cd75ab784a285cffb5b80")
	if hex.EncodeToString(id[:]) != "a3010fa5658e7e318cf00127f922bcb26c33c3bc211cd75ab784a285cffb5b80" {
		t.Error("mustID reverses the byte order — RPC and Header.ID() do not reverse it")
	}
}
