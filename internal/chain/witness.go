package chain

import (
	"errors"
	"fmt"
)

// The witness commitment.
//
// A block's id is the hash of its header, and the header's merkle root is built over txids — the
// transactions without their witnesses. Nothing in the id therefore says which signatures the
// miner put into the block: anybody who passes a block on can change a signature and the body
// still matches the header. A node that receives such a body cannot tell a tampered copy from an
// invalid block without validating all of it.
//
// From ForkWitnessCommitment on, the coinbase's last output is a data output that carries the
// merkle root over the wtxids of the block (WitnessMerkleRoot). The coinbase is under the header's
// merkle root, so the header now commits to the witnesses too, and the header itself keeps its
// 89 bytes. A body whose witnesses do not hash to the commitment is not the block the miner
// made, and that can be said on arrival, before anything is stored or validated.

const (
	// witnessCommitmentTag opens the commitment output's script; the root follows.
	witnessCommitmentTag = "PMwc"
	// WitnessCommitmentBytes is the length of that script.
	WitnessCommitmentBytes = len(witnessCommitmentTag) + 32
)

var (
	// ErrNoWitnessCommitment: the coinbase has no commitment output. That is part of the txid,
	// so no copy of the block can be valid.
	ErrNoWitnessCommitment = errors.New("chain: the coinbase does not commit to the witnesses")
	// ErrBadWitnessCommitment: the witnesses of this body are not the ones the coinbase commits
	// to. It says nothing about the block — another copy of it may match.
	ErrBadWitnessCommitment = errors.New("chain: the witnesses do not match the commitment in the coinbase")
)

func witnessCommitmentScript(root [32]byte) []byte {
	return append([]byte(witnessCommitmentTag), root[:]...)
}

// witnessCommitment reads the root the coinbase commits to.
func (b *Block) witnessCommitment() ([32]byte, error) {
	var root [32]byte
	if len(b.Txs) == 0 || b.Txs[0] == nil || len(b.Txs[0].Outputs) == 0 {
		return root, ErrNoWitnessCommitment
	}
	outs := b.Txs[0].Outputs
	last := outs[len(outs)-1]
	if last.ScriptVersion != ScriptVersionData || len(last.Script) != WitnessCommitmentBytes ||
		string(last.Script[:len(witnessCommitmentTag)]) != witnessCommitmentTag {
		return root, ErrNoWitnessCommitment
	}
	copy(root[:], last.Script[len(witnessCommitmentTag):])
	return root, nil
}

// checkWitnessesAgainst verifies the witnesses of this body against a commitment.
func (b *Block) checkWitnessesAgainst(want [32]byte) error {
	// The coinbase's leaf is not covered (WitnessMerkleRoot), so a witness on it would be bytes
	// anybody could add to a block.
	if len(b.Txs[0].Witness) != 0 {
		return fmt.Errorf("%w: the coinbase carries a witness", ErrBadWitnessCommitment)
	}
	if got := WitnessMerkleRoot(b.Txs); got != want {
		return fmt.Errorf("%w: they hash to %x, the coinbase says %x",
			ErrBadWitnessCommitment, got[:6], want[:6])
	}
	return nil
}

// CheckWitnesses verifies a body against the witness commitment of its own coinbase, where the
// rule holds; below the fork there is nothing to check and it returns nil.
//
// It needs no UTXO set, so it can run the moment a body arrives. ErrBadWitnessCommitment means
// the copy is not the miner's; ErrNoWitnessCommitment means the block itself is invalid.
func (b *Block) CheckWitnesses(params NetParams) error {
	if b.Header == nil || !params.Active(ForkWitnessCommitment, uint64(b.Header.Height)) {
		return nil
	}
	want, err := b.witnessCommitment()
	if err != nil {
		return err
	}
	return b.checkWitnessesAgainst(want)
}

// WitnessesCommitted says whether the header of this block vouches for the witnesses of this
// body: the rule holds at its height and they match the commitment. A body like that failing
// validation is the block's fault, never the copy's.
func (b *Block) WitnessesCommitted(params NetParams) bool {
	if b.Header == nil || !params.Active(ForkWitnessCommitment, uint64(b.Header.Height)) {
		return false
	}
	return b.CheckWitnesses(params) == nil
}
