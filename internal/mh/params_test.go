package mh

import "testing"

// Every epoch a chain can reach has a dataset to hash against: at least the size of epoch 0, at
// most the cap, never shrinking. An epoch that can only come from outside — a pool's job, a
// caller of the library — is told apart by ValidEpoch, because far enough out the size wraps
// around to nothing and a dataset of no items is a division by zero in every hash.
func TestEveryEpochAChainCanReachHasADataset(t *testing.T) {
	if MaxEpoch != EpochForHeight(1<<32-1) {
		t.Fatalf("MaxEpoch is %d, the last height a header can carry is in epoch %d", MaxEpoch, EpochForHeight(1<<32-1))
	}
	capAt := uint64((DagMaxBytes - DagBytesMainnetEpoch0) / DagGrowthPerEpoch)
	prev := 0
	for _, epoch := range []uint64{0, 1, 2, capAt - 1, capAt, capAt + 1, 100_000, MaxEpoch - 1, MaxEpoch} {
		if !ValidEpoch(epoch) {
			t.Fatalf("epoch %d is refused, a chain can reach it", epoch)
		}
		p := MainnetDag(epoch)
		if p.DatasetBytes < DagBytesMainnetEpoch0 || p.DatasetBytes > DagMaxBytes {
			t.Fatalf("epoch %d: a dataset of %d bytes", epoch, p.DatasetBytes)
		}
		if p.DatasetBytes < prev {
			t.Fatalf("epoch %d: the dataset shrank from %d to %d bytes", epoch, prev, p.DatasetBytes)
		}
		if p.DatasetItems() <= 0 || p.CacheItems() <= 0 {
			t.Fatalf("epoch %d: %d dataset items, %d cache items", epoch, p.DatasetItems(), p.CacheItems())
		}
		prev = p.DatasetBytes
	}

	// The epochs that must be refused, among them the one whose size comes out as zero bytes.
	for _, epoch := range []uint64{MaxEpoch + 1, 1 << 32, 1<<42 - 256, 1<<64 - 1} {
		if ValidEpoch(epoch) {
			t.Fatalf("epoch %d is accepted, no chain can reach it", epoch)
		}
	}
}
