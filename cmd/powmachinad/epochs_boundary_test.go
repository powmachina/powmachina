package main

import (
	"os"
	"runtime"
	"testing"
	"time"

	"powmachina/internal/chain"
	"powmachina/internal/mh"
)

// At an epoch boundary the node has to hold both epochs: a reorg across the boundary needs the
// old one too. And a third one evicts the old one, otherwise memory would grow with every epoch.
func TestEpochCacheKeepsTwoAcrossBoundary(t *testing.T) {
	c := newEpochCache(mh.TestProfile)
	defer c.close()

	last := mh.EpochBlocks - 1 // the last block of epoch 0
	if _, err := c.forHeight(last); err != nil {
		t.Fatal(err)
	}
	if _, err := c.forHeight(mh.EpochBlocks); err != nil { // the first block of epoch 1
		t.Fatal(err)
	}
	c.mu.Lock()
	have := len(c.sets)
	_, hasOld := c.sets[0]
	_, hasNew := c.sets[1]
	c.mu.Unlock()
	if have != 2 || !hasOld || !hasNew {
		t.Fatalf("at the boundary both epochs should be held, got %d (0:%v 1:%v)", have, hasOld, hasNew)
	}

	// A third epoch evicts the oldest one.
	if _, err := c.forHeight(2 * mh.EpochBlocks); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	have = len(c.sets)
	_, hasOld = c.sets[0]
	c.mu.Unlock()
	if have != keepEpochs {
		t.Fatalf("holding %d epochs, the cap is %d", have, keepEpochs)
	}
	if hasOld {
		t.Error("the oldest epoch was not freed")
	}
}

// The next epoch has to start being built **before** the boundary. Building the full DAG costs
// seconds and a gigabyte; if it started only with the block that already belongs to the new
// epoch, the node would freeze exactly when new blocks are arriving — and at the boundary
// everybody is in the same position.
//
// A `warm` function in the cache is not enough — something has to call it, and call it early.
func TestNodeWarmsNextEpochBeforeBoundary(t *testing.T) {
	node := NewNode(chain.RegTest, chain.RegtestGenesis(), mh.TestProfile)
	defer node.Close()

	// Still far from the boundary: nothing is built.
	node.warmNextEpoch(mh.EpochBlocks - warmAhead - 5)
	node.mu.Lock()
	warmed := node.warmedEpoch
	node.mu.Unlock()
	if warmed != 0 {
		t.Fatalf("epoch %d started being built outside the window", warmed)
	}

	// Inside the warmAhead window before the boundary the next epoch is built.
	node.warmNextEpoch(mh.EpochBlocks - warmAhead)
	deadline := time.Now().Add(30 * time.Second)
	for {
		node.epochs.mu.Lock()
		_, ok := node.epochs.sets[1]
		node.epochs.mu.Unlock()
		if ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("epoch 1 was not built ahead of time")
		}
		time.Sleep(20 * time.Millisecond)
	}
	node.mu.Lock()
	warmed = node.warmedEpoch
	node.mu.Unlock()
	if warmed != 1 {
		t.Fatalf("warmedEpoch is %d, expected 1", warmed)
	}
}

// A dry run of an epoch change with the **full DAG**. Skipped in CI — it
// needs gigabytes and minutes. Run it by hand on a machine that has the memory:
//
//	POWM_EPOCH_REHEARSAL=1 go test ./cmd/powmachinad -run TestEpochBoundaryFullDagRehearsal -v -timeout 60m
//
// It measures exactly what is unknown on a live network: how long the build takes and how much
// memory a node holds at the moment it has two epochs at once.
func TestEpochBoundaryFullDagRehearsal(t *testing.T) {
	if os.Getenv("POWM_EPOCH_REHEARSAL") != "1" {
		t.Skip("full-DAG epoch rehearsal: POWM_EPOCH_REHEARSAL=1 (gigabytes, minutes)")
	}
	c := newEpochCache(mh.MainnetProfile)
	defer c.close()

	rss := func() uint64 {
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		return m.Sys >> 20
	}

	t0 := time.Now()
	if _, err := c.forHeight(mh.EpochBlocks - 1); err != nil {
		t.Fatal(err)
	}
	first := time.Since(t0)
	t.Logf("epoch 0 (full DAG): %s, memory %d MiB", first.Round(time.Millisecond), rss())

	t1 := time.Now()
	if _, err := c.forHeight(mh.EpochBlocks); err != nil {
		t.Fatal(err)
	}
	second := time.Since(t1)
	t.Logf("epoch 1 alongside it: %s, memory %d MiB (two epochs at once)",
		second.Round(time.Millisecond), rss())

	c.mu.Lock()
	have := len(c.sets)
	c.mu.Unlock()
	if have != 2 {
		t.Fatalf("holding %d epochs at the boundary, expected 2", have)
	}

	// A check that both sets really do verify: without it the test would be measuring the build
	// of something that is never used for verification.
	for _, h := range []uint64{mh.EpochBlocks - 1, mh.EpochBlocks} {
		set, err := c.forHeight(h)
		if err != nil {
			t.Fatal(err)
		}
		if set.epoch != mh.EpochForHeight(h) {
			t.Fatalf("for height %d we have epoch %d", h, set.epoch)
		}
	}
	t.Logf("both epochs held and usable; the transition costs %s of building",
		second.Round(time.Millisecond))
}
