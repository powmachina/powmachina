package main

import (
	"testing"

	"powmachina/internal/mh"
)

// The epoch is the caller's number and it sizes the dataset. Past the last epoch a chain can reach
// the size is not defined — for some values it comes out as no items at all, and the first hash on
// such a context divides by zero and takes the caller's process with it. Such an epoch gets no
// context.
func TestOpenRefusesAnEpochNoChainCanReach(t *testing.T) {
	for _, network := range []string{"regtest", "testnet", "mainnet"} {
		for _, epoch := range []uint64{mh.MaxEpoch + 1, 1<<42 - 256, 1<<64 - 1} {
			if h := openContext(epoch, false, false, network); h != 0 {
				closeContext(h)
				t.Fatalf("%s: a context was opened for epoch %d", network, epoch)
			}
		}
	}
	h := openContext(mh.MaxEpoch, false, false, "regtest")
	if h == 0 {
		t.Fatal("the last epoch a chain can reach was refused")
	}
	closeContext(h)
}
