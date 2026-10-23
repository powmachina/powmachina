package chain

import "powmachina/internal/mh"

// The genesis blocks.
//
// The mainnet genesis is fixed only before the network launches (SPEC §5) — until then it is not
// here, so a placeholder cannot be used by mistake as the real root of trust.

// TestnetGenesis is the testnet's root. The time is 2026-10-23 08:00 UTC — the hour the network
// started, so the chain's age is readable from the chain itself. The root changes whenever
// consensus changes in a way no fork can carry, even though it would not have to: a node with the
// old binary and one with the new would otherwise shake hands (the same genesis and magic) and
// then reject each other's blocks. With a different root they do not connect and the reason is
// visible.
//
// The genesis has **no PoW and no body**, the same as on regtest: consensus does not verify it, it
// is the root of trust written into the binary (`HeaderChain` takes it as given). Searching for a
// nonce for it would be theatre — nobody would check it. From block 1 on, everything applies.
//
// It differs from regtest by the network magic (`PWMT`) and by the difficulty: testnet runs on the
// mainnet parameters, so it measures what will one day run for real.
func TestnetGenesis() *Header {
	return &Header{
		Version: 1,
		Time:    1_792_742_400, // 2026-10-23T08:00:00Z
		Bits:    MaxTargetBits,
		Height:  0,
		Nonce:   0,
		Lane:    mh.LaneC,
	}
}

// Genesis returns a given network's root. Mainnet does not have one yet — it returns `nil` rather
// than a placeholder, so it cannot be used by mistake as a real network's root of trust (SPEC §5).
func Genesis(p NetParams) *Header {
	switch p.Magic {
	case TestNet.Magic:
		return TestnetGenesis()
	case RegTest.Magic:
		return RegtestGenesis()
	}
	return nil
}

// RegtestGenesis is the regtest network's root. It has no PoW and no reward; it is just a fixed
// point to count from.
func RegtestGenesis() *Header {
	return &Header{
		Version: 1,
		Time:    1_700_000_000,
		Bits:    RegtestTargetBits,
		Height:  0,
		Nonce:   0,
		Lane:    mh.LaneC,
	}
}
