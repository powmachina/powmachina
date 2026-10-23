package chain

// Subsidy returns the block reward in cogs, including the dev fee.
//
// The genesis coinbase is unspendable and has a zero reward (fair launch, no premine).
func Subsidy(height uint64) int64 {
	if height == 0 {
		return 0
	}
	halvings := height / HalvingInterval
	base := int64(0)
	if halvings < 63 {
		base = InitialSubsidy >> halvings
	}
	// The permanent reward: after the slow start, never less than TailSubsidy.
	if height >= SlowStartBlocks && base < TailSubsidy {
		return TailSubsidy
	}

	if height < SlowStartBlocks {
		// A linear ramp from 1 % to 100 % over the first 20 000 blocks (anti-instamine).
		num := height + 1
		if min := SlowStartBlocks / 100; num < min {
			num = min
		}
		return base * int64(num) / int64(SlowStartBlocks)
	}
	return base
}

// DevFee returns the part of the reward that has to go to DEV_FEE_SCRIPT — 5 % of the subsidy up
// to height DevFeeEndHeight (≈ 2 years), then nothing.
func DevFee(height uint64) int64 {
	if height == 0 || height >= DevFeeEndHeight {
		return 0
	}
	return Subsidy(height) * DevFeeBps / 10_000
}

// MinerSubsidy is what is left of the reward for the miner (without transaction fees).
func MinerSubsidy(height uint64) int64 { return Subsidy(height) - DevFee(height) }
