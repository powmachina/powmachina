// Package chain carries PowMachina's consensus rules: the header, emission, difficulty and the
// lanes.
//
// The values match the chain spec. Whatever is a constant here is part of consensus — it may only
// be changed together with the spec, and after the genesis not at all.
package chain

import "time"

const (
	// BlockTime is the network's target block time.
	BlockTime = 120 * time.Second
	// LaneBlockTime is the target block time of a single lane: two lanes × 240 s = 120 s.
	//
	// It is derived from the number of lanes, so it has to be recomputed whenever a lane is added
	// or removed — otherwise the emission axis shifts silently (halving is in blocks, not years).
	// TestLaneBlockTimeMatchesLaneCount guards this.
	LaneBlockTime = 240 * time.Second

	// MaxBlockBytes is the cap on a serialized block. Together with the block time it sets the
	// chain's growth: 256 kB / 120 s ≈ 2.13 kB/s ≈ 69 GB/year at full use.
	MaxBlockBytes = 256 * 1024
	// MaxCoinbaseBytes leaves room for a decentralised pool's payouts
	// (the sharechain note §4: 512 outputs ≈ 21.6 kB).
	MaxCoinbaseBytes = 64 * 1024

	// Cog is the smallest unit; 1 POWM = 10^8 cogs.
	Cog  int64 = 1
	POWM int64 = 100_000_000
	// HalvingSupply is how much is emitted through halvings before the permanent reward takes over
	// (~84 M POWM; the slow start makes it slightly less). It is not a cap — with the permanent
	// reward the supply keeps growing slowly (the emission-tail note).
	HalvingSupply = 84_000_000 * POWM
	// MaxMoney is the upper bound for checking sums of values (protection against int64 overflow),
	// not the emission. 10⁹ POWM is only reached by the permanent reward after thousands of years.
	MaxMoney = 1_000_000_000 * POWM

	// TailSubsidy is the permanent minimum block reward: once halving would drop below it, the
	// reward stays there forever — miners always have something to live on and fees can stay cheap.
	// 0.999 POWM = the sum of the ASCII codes of the letters in "PowMachina" (999). It takes over
	// at the sixth halving (~year 24), inflation ~0.3 % and falling.
	// TODO(before genesis): confirm finally (proposal: the community during testnet).
	TailSubsidy int64 = 99_900_000

	// InitialSubsidy is the block reward before the first halving.
	InitialSubsidy = 40 * POWM
	// HalvingInterval ≈ 4 years at a 120 s block.
	HalvingInterval uint64 = 1_050_000
	// SlowStartBlocks: for the first 5 000 blocks the reward grows linearly (anti-instamine, ≈ 7 days).
	SlowStartBlocks uint64 = 5_000

	// DevFeeBps is 5 % of the subsidy, DevFeeEndHeight ≈ 2 years.
	DevFeeBps       int64  = 500
	DevFeeEndHeight uint64 = 525_000

	// CoinbaseMaturity is the number of blocks after which a coinbase can be spent (≈ 3.3 h).
	CoinbaseMaturity uint64 = 100
	// ReorgLimit is the depth beyond which a reorg is not accepted (≈ 16.7 h).
	ReorgLimit uint64 = 500

	// EpochBlocks is the epoch of the DAG and the RANDMATH sequence (≈ 6.9 days).
	EpochBlocks uint64 = 5_000

	// LwmaWindow is the retarget window, counted in blocks of the same lane (60 × 240 s = 4 h).
	LwmaWindow = 60
	// MedianTimeSpan is the number of blocks for the median time past.
	MedianTimeSpan = 11
	// MaxFutureDrift is how far into the future a block timestamp may go.
	MaxFutureDrift = 120 * time.Second

	// MaxSameLaneRun is the lane rotation rule: at most two blocks of the same lane in a row.
	MaxSameLaneRun = 2
	// LaneRotationRelief waives the rotation rule when the network stalls.
	LaneRotationRelief = 900 * time.Second
)

// EpochForHeight returns the DAG epoch for a given height.
func EpochForHeight(height uint64) uint64 { return height / EpochBlocks }
