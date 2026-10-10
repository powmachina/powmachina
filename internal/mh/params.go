package mh

// The MachinaHash MH-1 parameters.
//
// CAREFUL: the values are a PROPOSAL from before calibration (the algorithm spec §7). Until
// F2/F3 have run, any of them may change — which is why they are all in one place.
//
// Every one of these constants is consensus. Changing any of them = a different network.

const (
	// DagItemBytes is a DAG item = 256 B = 64 × uint32 (two 128 B cache line accesses).
	DagItemBytes = 256
	DagItemWords = DagItemBytes / 4
	// CacheItemBytes is a cache item = 64 B = 16 × uint32.
	CacheItemBytes = 64
	CacheItemWords = CacheItemBytes / 4

	// MixWords is the width of the B-core mix state = 256 B = 64 × uint32 (as wide as a DAG item).
	MixWords = 64

	// BAccessesPerRound is the default number of independent DAG accesses per round.
	BAccessesPerRound = 64
	// DagParents is the number of parents when generating one DAG item from the cache —
	// this is the cost of a light verify.
	DagParents = 256
	// CacheRounds is the number of randmemohash passes when generating the cache.
	CacheRounds = 2
	// RandMathOps is the length of the RANDMATH sequence (it changes every epoch).
	RandMathOps = 16

	// EpochBlocks is the epoch = 5 000 blocks (≈ 6.9 days with a 120 s block).
	EpochBlocks uint64 = 5000

	// CacheBytesMainnet is the mainnet cache: 16 MiB.
	CacheBytesMainnet = 16 << 20
	// DagBytesMainnetEpoch0 is the mainnet DAG in epoch 0: 1 GiB.
	DagBytesMainnetEpoch0 = 1 << 30
	// DagGrowthPerEpoch is the DAG's growth per epoch: +4 MiB.
	//
	// The growth is meant to stop an ASIC with fixed memory, but it must not retire cards — Ethash
	// retired 4 GB GPUs that way. With a 120 s block it is ≈ 210 MiB/year: 2.0 GiB after 5 years,
	// 3.1 GiB after 10.
	DagGrowthPerEpoch = 4 << 20
	// DagMaxBytes is the cap on the DAG's size: 8 GiB. With growth of
	// +4 MiB/epoch it is reached after about 34 years.
	//
	// Above ~4 GiB (≈ after 14.5 years) 4–6 GB graphics cards, 8 GB laptops and SBCs drop out. If
	// that is a problem at the time, it is solved with a planned tweak, not by changing this cap
	// retroactively.
	DagMaxBytes = 8 << 30

	// HeaderBytes is the size of the block header the PoW is computed over.
	HeaderBytes = 80

	fnvPrime = 0x01000193
)

// Domain separation — so a hash from PowMachina cannot be carried elsewhere.
var (
	domainSeed    = []byte("PowMachina/MH-1/seed/v0")
	domainRxKey   = []byte("PowMachina/MH-C/key/v0")
	domainFinal   = []byte("PowMachina/MH-1/final/v0")
	domainBcore   = []byte("PowMachina/MH-1/bcore/v0")
	domainEpoch   = []byte("PowMachina/MH-1/epoch/v0")
	domainCache   = []byte("PowMachina/MH-1/cache/v0")
	domainItem    = []byte("PowMachina/MH-1/item/v0")
	domainBlockID = []byte("PowMachina/block-id/v0")
)

func fnv1a(h, d uint32) uint32 { return (h ^ d) * fnvPrime }

// dagBytesForEpoch is the DAG's size for a given epoch (the mainnet profile), capped at DagMaxBytes.
func dagBytesForEpoch(epoch uint64) int {
	grown := DagBytesMainnetEpoch0 + int(epoch)*DagGrowthPerEpoch
	if grown > DagMaxBytes || grown < 0 {
		return DagMaxBytes
	}
	return grown
}

// EpochForHeight returns the epoch for a given block height. It applies to both lane G's DAG and lane
// C's RandomX key.
func EpochForHeight(height uint64) uint64 { return height / EpochBlocks }
