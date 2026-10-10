package chain

import (
	"sync"
	"testing"
)

// Several goroutines push into the finality state at once: every peer's read loop and RPC from the
// standalone signer. Without a lock that ends as a crash on the map.
//
// The test matters mainly with -race, but even without it, it catches lost attestations.
func TestFinalityStateConcurrentAdd(t *testing.T) {
	f := NewFinalityState(RegTest.SignerSet())
	const heights = 30
	ids := make(map[uint64][32]byte, heights)
	for h := uint64(1); h <= heights; h++ {
		var id [32]byte
		id[0], id[1] = byte(h), 0xAB
		ids[h] = id
	}
	lookup := func(h uint64) ([32]byte, bool) {
		id, ok := ids[h]
		return id, ok
	}

	var wg sync.WaitGroup
	for i := 0; i < FinalitySigners; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			for h := uint64(1); h <= heights; h++ {
				att := Sign(RegtestSignerKey(idx), uint8(idx), h, ids[h])
				// Twice: the second one is a duplicate and must not break anything.
				_, _ = f.Add(att)
				_, _ = f.Add(att)
				f.Update(heights+FinalityDepth, lookup)
			}
		}(i)
	}
	// Reading alongside the writes — that is exactly what RPC and a reorg do.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			_, _, _ = f.Finalized()
			_ = f.CheckReorg(heights)
		}
	}()
	wg.Wait()

	f.Update(heights+FinalityDepth, lookup)
	got, id, ok := f.Finalized()
	if !ok || got != heights || id != ids[heights] {
		t.Fatalf("finality is %d (%v), expected %d", got, ok, heights)
	}
}
