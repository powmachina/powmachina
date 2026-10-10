package chain

import "math"

// Storage mass — protection against dust spam in consensus.
//
// Taken from Kaspa (KIP-9), which introduced it after the dust attack of September 2023: in every
// block the attacker broke one coin into hundreds of tiny ones and its list of unspent coins grew
// from 8 to 62 million. Details and the choice of constants: the fees-and-dust note.
//
//	storage_mass(tx) = C × ( Σ_o 1/o  −  |I|² / Σ_i i )⁺
//	relaxed, if |O| ≤ |I| ≤ 2 or |O| = 1:   C × ( Σ_o 1/o − Σ_i 1/i )⁺
//
// o, i are the values of the outputs and inputs in cogs; data outputs do not count.
//
// The properties the whole thing rests on — and which hold for an attacker who knows this code too:
//   - an ordinary payment with normal amounts has a negligible mass,
//   - consolidating coins has zero mass,
//   - many tiny outputs have an enormous mass: inflating the coin list by N entries costs a budget
//     that grows with N² — it cannot be worked around by splitting into several transactions,
//   - it holds for every block, so it also holds for a miner who puts their own spam into their own block.
//
// Integer arithmetic only (project rule 2). Division rounds down — the rule is deterministic and
// the same on every node, so the rounding does not matter.
const (
	// StorageMassC is a consensus constant. 10⁸: an output of 1 POWM has mass 1, an output of 1 cog 10⁸.
	// Tightening it (raising it) is possible with a soft fork, relaxing it only with a hard fork — which is why it is not stricter.
	StorageMassC uint64 = 100_000_000
	// MaxBlockStorageMass is the cap on the sum of masses in a block, including the coinbase.
	MaxBlockStorageMass uint64 = 256_000
	// CoinbaseStorageMassReserve is the room a block template leaves for the coinbase.
	CoinbaseStorageMassReserve uint64 = 16_000
)

// StorageMass computes a transaction's storage mass with the constant `c`.
// `inValues` are the values of the coins on the input side (empty for a coinbase).
//
// An output with a zero value (other than a data one) has infinite mass — it does not fit into a block.
func StorageMass(inValues []int64, outs []TxOut, c uint64) uint64 {
	var outTerm uint64
	var nOut uint64
	for _, o := range outs {
		if o.ScriptVersion == ScriptVersionData {
			continue
		}
		if o.Value <= 0 {
			return math.MaxUint64
		}
		outTerm = satAdd(outTerm, c/uint64(o.Value))
		nOut++
	}
	nIn := uint64(len(inValues))

	var inTerm uint64
	if nIn > 0 {
		if (nOut <= nIn && nIn <= 2) || nOut == 1 {
			// the relaxed formula: a harmonic mean on the input side too
			for _, v := range inValues {
				if v > 0 {
					inTerm = satAdd(inTerm, c/uint64(v))
				}
			}
		} else {
			var sum uint64
			for _, v := range inValues {
				if v > 0 {
					sum = satAdd(sum, uint64(v))
				}
			}
			if sum > 0 {
				// |I|² × C / Σ i — |I| ≤ MaxTxInputs, so |I|² × C fits into a uint64 for every C up
				// to ~1.8·10¹³; we saturate anyway, so that it always holds.
				inTerm = satMul(satMul(nIn, nIn), c) / sum
			}
		}
	}
	if inTerm >= outTerm {
		return 0
	}
	return outTerm - inTerm
}

// TxStorageMass is a transaction's mass with the consensus constant.
func TxStorageMass(inValues []int64, tx *Tx) uint64 {
	return StorageMass(inValues, tx.Outputs, StorageMassC)
}

func satAdd(a, b uint64) uint64 {
	if a > math.MaxUint64-b {
		return math.MaxUint64
	}
	return a + b
}

func satMul(a, b uint64) uint64 {
	if a != 0 && b > math.MaxUint64/a {
		return math.MaxUint64
	}
	return a * b
}
