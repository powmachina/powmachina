package stratum

import (
	"strings"
	"testing"

	"powmachina/internal/mh"
)

// The epoch in a job is the server's word, and the miner builds a dataset of the size that epoch
// has. An epoch no chain can reach is refused with the job — among them the ones whose size wraps
// around to a dataset of no items, which the first hash would divide by.
func TestJobWithAnEpochNoChainCanReachIsRefused(t *testing.T) {
	p := JobParams{JobID: "G1", Lane: "G", Height: 1,
		Header: strings.Repeat("00", mh.HeaderBytes), Target: strings.Repeat("ff", 32)}

	for _, epoch := range []uint64{0, 1, mh.MaxEpoch} {
		p.Epoch = epoch
		j, err := JobFrom(&p)
		if err != nil {
			t.Fatalf("epoch %d: %v", epoch, err)
		}
		if j.Epoch != epoch {
			t.Fatalf("the job carries epoch %d, the server sent %d", j.Epoch, epoch)
		}
	}
	for _, epoch := range []uint64{mh.MaxEpoch + 1, 1<<42 - 256, 1<<64 - 1} {
		p.Epoch = epoch
		if _, err := JobFrom(&p); err == nil {
			t.Fatalf("a job for epoch %d was accepted", epoch)
		}
	}
}
