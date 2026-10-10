package chain

import (
	"encoding/binary"

	"powmachina/internal/mh"
)

// The sighash — what exactly the signer commits to.
//
// It commits to **every input including their amounts and scripts** and to every output. The
// amounts are there on purpose: without them a different input value can be slipped to the signer
// and a fee lured out of them, which Bitcoin got burned by in the past (and which is why BIP143
// came about).
//
// For now there is one mode only, the equivalent of SIGHASH_ALL. Further modes are needless surface
// for bugs — when they are needed, they will be added through a new script version.

const (
	sighashTag          = "PowMachina/sighash/v0"
	sighashPrevoutsTag  = "PowMachina/sighash-prevouts/v0"
	sighashAmountsTag   = "PowMachina/sighash-amounts/v0"
	sighashScriptsTag   = "PowMachina/sighash-scripts/v0"
	sighashSequencesTag = "PowMachina/sighash-sequences/v0"
	sighashOutputsTag   = "PowMachina/sighash-outputs/v0"
)

// SigHash computes what input `index` signs. `coins` are the outputs the transaction spends, in the
// same order as the inputs.
func SigHash(tx *Tx, index int, coins []Coin) [32]byte {
	var b [8]byte

	prevouts := make([]byte, 0, len(tx.Inputs)*36)
	sequences := make([]byte, 0, len(tx.Inputs)*4)
	amounts := make([]byte, 0, len(tx.Inputs)*8)
	scripts := make([]byte, 0, len(tx.Inputs)*40)
	for i, in := range tx.Inputs {
		prevouts = append(prevouts, in.PrevTxID[:]...)
		binary.LittleEndian.PutUint32(b[:4], in.PrevIndex)
		prevouts = append(prevouts, b[:4]...)

		binary.LittleEndian.PutUint32(b[:4], in.Sequence)
		sequences = append(sequences, b[:4]...)

		var value int64
		var version uint8
		var script []byte
		if i < len(coins) {
			value, version, script = coins[i].Value, coins[i].ScriptVersion, coins[i].Script
		}
		binary.LittleEndian.PutUint64(b[:], uint64(value))
		amounts = append(amounts, b[:]...)

		scripts = append(scripts, version)
		binary.LittleEndian.PutUint16(b[:2], uint16(len(script)))
		scripts = append(scripts, b[:2]...)
		scripts = append(scripts, script...)
	}

	outputs := make([]byte, 0, len(tx.Outputs)*48)
	for _, o := range tx.Outputs {
		binary.LittleEndian.PutUint64(b[:], uint64(o.Value))
		outputs = append(outputs, b[:]...)
		outputs = append(outputs, o.ScriptVersion)
		binary.LittleEndian.PutUint16(b[:2], uint16(len(o.Script)))
		outputs = append(outputs, b[:2]...)
		outputs = append(outputs, o.Script...)
	}

	hPrevouts := mh.TaggedHash(sighashPrevoutsTag, prevouts)
	hAmounts := mh.TaggedHash(sighashAmountsTag, amounts)
	hScripts := mh.TaggedHash(sighashScriptsTag, scripts)
	hSequences := mh.TaggedHash(sighashSequencesTag, sequences)
	hOutputs := mh.TaggedHash(sighashOutputsTag, outputs)

	msg := make([]byte, 0, 4+4+32*5+4)
	binary.LittleEndian.PutUint32(b[:4], tx.Version)
	msg = append(msg, b[:4]...)
	binary.LittleEndian.PutUint32(b[:4], tx.LockTime)
	msg = append(msg, b[:4]...)
	msg = append(msg, hPrevouts[:]...)
	msg = append(msg, hAmounts[:]...)
	msg = append(msg, hScripts[:]...)
	msg = append(msg, hSequences[:]...)
	msg = append(msg, hOutputs[:]...)
	binary.LittleEndian.PutUint32(b[:4], uint32(index))
	msg = append(msg, b[:4]...)

	return mh.TaggedHash(sighashTag, msg)
}
