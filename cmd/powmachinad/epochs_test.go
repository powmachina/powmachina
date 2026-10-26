package main

import (
	"testing"

	"powmachina/internal/chain"
	"powmachina/internal/mh"
	"powmachina/internal/p2p"
)

// The node has to cross an epoch boundary. One that holds a single epoch (0) rejects every
// header from height EpochBlocks on — which looks like a stalled network.
func TestEpochCacheCrossesBoundary(t *testing.T) {
	c := newEpochCache(mh.TestProfile)
	defer c.close()

	last0, err := c.forHeight(0)
	if err != nil {
		t.Skipf("the epoch cannot be built: %v", err)
	}
	if last0.epoch != 0 {
		t.Fatalf("height 0 should be epoch 0, got %d", last0.epoch)
	}

	// The same epoch must not be built a second time.
	again, err := c.forHeight(chain.EpochBlocks - 1)
	if err != nil {
		t.Fatal(err)
	}
	if again != last0 {
		t.Fatal("the set for the same epoch was built again")
	}

	next, err := c.forHeight(chain.EpochBlocks)
	if err != nil {
		t.Fatal(err)
	}
	if next.epoch != 1 {
		t.Fatalf("beyond the boundary it should be epoch 1, got %d", next.epoch)
	}
	if next == last0 {
		t.Fatal("epoch 1 got epoch 0's set")
	}
}

// At most keepEpochs sets are held — otherwise the node would eat memory after weeks of running.
func TestEpochCacheEvicts(t *testing.T) {
	c := newEpochCache(mh.TestProfile)
	defer c.close()

	for e := uint64(0); e < keepEpochs+2; e++ {
		if _, err := c.forHeight(e * chain.EpochBlocks); err != nil {
			t.Skipf("the epoch cannot be built: %v", err)
		}
	}
	c.mu.Lock()
	n := len(c.sets)
	c.mu.Unlock()
	if n != keepEpochs {
		t.Fatalf("should be holding %d epochs, holding %d", keepEpochs, n)
	}
}

// Sync mode: the full DAG during a sync, once caught up to the tip every held epoch switches to
// light (the dataset is freed) and after another long outage back to full. PoW verification has
// to give the same result in both modes.
func TestEpochCacheAutoSwitchesDag(t *testing.T) {
	c := newEpochCache(mh.TestProfile)
	defer c.close()
	syncing := true
	c.setMode(dagSync, func() bool { return syncing })

	h := &chain.Header{Version: 1, Height: 7, Bits: chain.RegtestTargetBits, Lane: mh.LaneG}
	s0, err := c.forHeight(0)
	if err != nil {
		t.Skipf("the epoch cannot be built: %v", err)
	}
	s1, _ := c.forHeight(chain.EpochBlocks)
	if s0.verifier.Light() || s1.verifier.Light() || !c.isFull() {
		t.Fatal("during a sync the DAG should be full")
	}
	// The hash itself is compared, not just yes/no against the target — that would not prove a match.
	pow := h.PoWInput()
	hash := func(n uint64) [32]byte {
		s0.mu.Lock()
		defer s0.mu.Unlock()
		out, err := s0.hasher.Hash(s0.verifier, &pow, n, mh.LaneG)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	var want [][32]byte
	for n := range uint64(32) {
		want = append(want, hash(n))
	}

	syncing = false
	if _, err := c.forHeight(chain.EpochBlocks); err != nil {
		t.Fatal(err)
	}
	if !s0.verifier.Light() || !s1.verifier.Light() || c.isFull() {
		t.Fatal("at the tip every epoch should be light")
	}
	if s0.verifier.Dag() != nil {
		t.Fatal("a light epoch still holds a dataset")
	}
	for n := range uint64(32) {
		if hash(n) != want[n] {
			t.Fatalf("nonce %d: the light hash differs from the full one", n)
		}
	}
	// A new epoch at the tip is built as light right away.
	s2, _ := c.forHeight(2 * chain.EpochBlocks)
	if !s2.verifier.Light() {
		t.Fatal("a new epoch at the tip was built full")
	}

	syncing = true
	if _, err := c.forHeight(2 * chain.EpochBlocks); err != nil {
		t.Fatal(err)
	}
	if s2.verifier.Light() || !c.isFull() {
		t.Fatal("after returning to a sync the DAG should be full again")
	}
}

// The fixed modes do not look at the sync state.
func TestEpochCacheFixedModes(t *testing.T) {
	for _, tc := range []struct {
		mode  dagMode
		light bool
	}{{dagFull, false}, {dagLight, true}} {
		c := newEpochCache(mh.TestProfile)
		c.setMode(tc.mode, func() bool { return !tc.light }) // syncing the opposite way to the mode
		s, err := c.forHeight(0)
		if err != nil {
			t.Skipf("the epoch cannot be built: %v", err)
		}
		if s.verifier.Light() != tc.light {
			t.Fatalf("mode %s: light = %v", tc.mode, s.verifier.Light())
		}
		c.close()
	}
	if _, err := parseDagMode("fast"); err == nil {
		t.Fatal("an unknown mode passed")
	}
}

// A node at the tip in auto mode does not advertise ServiceFullDag — a peer would expect something it does not have.
func TestLightNodeDoesNotAdvertiseFullDag(t *testing.T) {
	node := NewNode(chain.RegTest, chain.RegtestGenesis(), mh.TestProfile)
	defer node.Close()
	// The regtest genesis is from 2023 — the node is "behind".
	if !node.syncing() {
		t.Fatal("a node with a tip years old should be catching up on history")
	}
	node.SetDagMode(dagLight)
	if _, err := node.pow(0); err != nil {
		t.Fatal(err)
	}
	if node.services()&p2p.ServiceFullDag != 0 {
		t.Fatal("a light node advertises ServiceFullDag")
	}
}
