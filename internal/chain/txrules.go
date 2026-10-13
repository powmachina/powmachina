package chain

import (
	"errors"
	"fmt"
)

// The rules of a transaction that depend on where it goes: the network and the height of the
// block. CheckSanity is what holds for a transaction anywhere; this is the rest.

var (
	// ErrTxLocked: the transaction's lock time has not come yet.
	ErrTxLocked = errors.New("chain: the transaction is locked until a later height")
	// ErrTxBadSequence: the sequence field is reserved and has to be zero.
	ErrTxBadSequence = errors.New("chain: the sequence of an input is reserved and has to be zero")
)

// CheckRules verifies a transaction against the rules of the block it is meant for.
//
// ForkLockTime — the two fields a transaction always had and nothing enforced:
//
//   - LockTime is a height: the transaction may go into a block at that height or above, and into
//     none below. Zero is no lock. A lock by wall-clock time was left out: a height says the same
//     thing at 120 s a block, and it needs nothing but the block's own header to check.
//   - Sequence is reserved and has to be zero. It is kept in the format — and in what is signed —
//     so that a later fork can give it a meaning (a lock relative to the spent output is what
//     payment channels need) without changing how a transaction is laid out.
//
// ForkStrictOutputs — an output is something the chain knows how to spend:
//
//   - a version 0 output is exactly a 32-byte key. Any other length could never be spent, and an
//     output nobody can spend stays in the UTXO set of every node for good;
//   - an output of a reserved version (1–15) cannot be created until a fork defines the version.
//     Before, such an output could be spent by anybody, so whatever was sent to one by mistake
//     belonged to the first miner to notice.
func (t *Tx) CheckRules(rules Rules) error {
	if rules.Active(ForkLockTime) {
		for i, in := range t.Inputs {
			if in.Sequence != 0 {
				return fmt.Errorf("%w (input %d)", ErrTxBadSequence, i)
			}
		}
		if uint64(t.LockTime) > rules.Height {
			return fmt.Errorf("%w: height %d, the block is %d", ErrTxLocked, t.LockTime, rules.Height)
		}
	}
	if rules.Active(ForkStrictOutputs) {
		for i, o := range t.Outputs {
			switch {
			case o.ScriptVersion == ScriptVersionBasic:
				if len(o.Script) != PubkeyBytes {
					return fmt.Errorf("%w (output %d has %d B)", ErrBadPubkeyScript, i, len(o.Script))
				}
			case o.ScriptVersion <= ScriptVersionMaxReserved:
				return fmt.Errorf("%w: output %d has version %d, which is not defined yet",
					ErrTxBadVersion, i, o.ScriptVersion)
			}
		}
	}
	return nil
}
