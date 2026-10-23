// Package chain carries PowMachina's consensus rules: the header, emission, difficulty and the
// lanes.
//
// The values match the chain spec. Whatever is a constant here is part of consensus — it may only
// be changed together with the spec, and after the genesis not at all.
package chain

import (
	"time"

	"powmachina/internal/mh"
)

const (
	// BlockTime is the network's target block time. 30 s, so that a share settles quickly and a
	// small miner sees its credit within a minute (the Sharepool spec §8); every rule measured in
	// time stays where it was at 120 s, whatever is counted in blocks is four times larger.
	BlockTime = 30 * time.Second
	// LaneBlockTime is the target block time of a single lane: two lanes × 60 s = 30 s.
	//
	// It is derived from the number of lanes, so it has to be recomputed whenever a lane is added
	// or removed — otherwise the emission axis shifts silently (halving is in blocks, not years).
	// TestLaneBlockTimeMatchesLaneCount guards this.
	LaneBlockTime = 60 * time.Second

	// MaxBlockBytes is the cap on a serialized block. Together with the block time it sets the
	// chain's growth: 96 kB / 30 s ≈ 3.2 kB/s ≈ 100 GB/year at full use, of which transactions
	// are what MaxCoinbaseBytes leaves.
	MaxBlockBytes = 96 * 1024
	// MaxCoinbaseBytes leaves room for the payouts of the Sharepool (the Sharepool spec §5.2:
	// 400 outputs × 43 B ≈ 17 kB, and the commitments).
	MaxCoinbaseBytes = 24 * 1024

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
	// 0.999 POWM every 2 minutes — 999 is the sum of the ASCII codes of the letters in
	// "PowMachina" — is 0.24975 POWM a 30 s block. It takes over at the sixth halving (~year 24),
	// inflation ~0.3 % and falling.
	TailSubsidy int64 = 24_975_000

	// InitialSubsidy is the block reward before the first halving.
	InitialSubsidy = 10 * POWM
	// HalvingInterval ≈ 4 years at a 30 s block.
	HalvingInterval uint64 = 4_200_000
	// SlowStartBlocks: for the first 20 000 blocks the reward grows linearly (anti-instamine, ≈ 7 days).
	SlowStartBlocks uint64 = 20_000

	// DevFeeBps is 5 % of the subsidy, DevFeeEndHeight ≈ 2 years.
	DevFeeBps       int64  = 500
	DevFeeEndHeight uint64 = 2_100_000

	// CoinbaseMaturity is the number of blocks after which a coinbase can be spent (≈ 3.3 h).
	CoinbaseMaturity uint64 = 400
	// ReorgLimit is the depth beyond which a reorg is not accepted (≈ 16.7 h).
	ReorgLimit uint64 = 2_000

	// EpochBlocks is the epoch of the DAG and the RANDMATH sequence (≈ 6.9 days). The algorithm
	// owns it; the chain only reads it.
	EpochBlocks = mh.EpochBlocks

	// LwmaWindow is the retarget window before ForkLaneDifficulty, counted in blocks of the same
	// lane (60 × 60 s = 1 h). Only a network without the fork uses it.
	LwmaWindow = 60
	// MedianTimeSpan is the number of blocks for the median time past.
	MedianTimeSpan = 11
	// MaxFutureDrift is how far into the future a block timestamp could go before
	// ForkLaneDifficulty.
	MaxFutureDrift = 120 * time.Second
	// MaxFutureDriftStrict is the bound from ForkLaneDifficulty on (NetParams.FutureDrift). The
	// lane rotation relief and the lane retarget are both measured in block times, so whatever a
	// miner may add to its clock it may take off the relief; NTP keeps a machine within a second.
	MaxFutureDriftStrict = 30 * time.Second

	// MaxSameLaneRun is the lane rotation rule: at most two blocks of the same lane in a row.
	MaxSameLaneRun = 2
	// LaneRotationRelief waives the rotation rule when the network stalls. 300 s = 5 lane blocks:
	// a third block of a lane at rest gets through in 0.03 % of blocks, a chain with one lane gone
	// makes a block every 6 minutes, and a block dated MaxFutureDriftStrict ahead leaves 270 s of
	// it that have to be real (`powm-diffsim -spacing 60 -relief 300`, the Sharepool spec §8).
	LaneRotationRelief = 300 * time.Second
)

// EpochForHeight returns the DAG epoch for a given height.
func EpochForHeight(height uint64) uint64 { return height / EpochBlocks }
