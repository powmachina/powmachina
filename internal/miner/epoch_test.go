package miner

import (
	"log"
	"strings"
	"testing"

	"powmachina/internal/chain"
	"powmachina/internal/mh"
	"powmachina/internal/work"
)

// A job names its epoch and the miner builds for it: the cache and the dataset for the card,
// RandomX for the CPU. An epoch no chain can reach is not built for — the job is dropped and the
// lane waits for the next one. Among those epochs are the ones whose dataset comes out with no
// items, and hashing against that is a division by zero.
func TestJobForAnEpochNoChainCanReachIsNotMined(t *testing.T) {
	var buf syncBuf
	m := New(Options{Params: chain.RegTest, Threads: -1, GPUs: []GPU{&fakeGPU{}}, Log: log.New(&buf, "", 0)})
	l := m.lanes[mh.LaneG]

	good := &work.Job{Lane: mh.LaneG, Epoch: 0}
	m.setJob(mh.LaneG, good)
	if tk := l.job.Load(); tk == nil || tk.job != good {
		t.Fatal("a job for epoch 0 was not taken")
	}
	built := m.epochG

	for _, epoch := range []uint64{mh.MaxEpoch + 1, 1<<42 - 256, 1<<64 - 1} {
		m.setJob(mh.LaneG, &work.Job{Lane: mh.LaneG, Epoch: epoch})
		if l.job.Load() != nil {
			t.Fatalf("a job for epoch %d is being mined", epoch)
		}
		if m.epochG != built {
			t.Fatalf("a cache was built for epoch %d", epoch)
		}
	}
	if !strings.Contains(buf.String(), "no chain can reach") {
		t.Fatalf("the log does not say why the lane stopped:\n%s", buf.String())
	}

	// The next good job is mined as if nothing had happened.
	m.setJob(mh.LaneG, good)
	if tk := l.job.Load(); tk == nil || tk.job != good {
		t.Fatal("the lane did not take a good job after a bad one")
	}
}
