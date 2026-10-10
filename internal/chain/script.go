package chain

import (
	"errors"
	"fmt"

	"powmachina/internal/mh"
)

// Verifying the spending of an output.
//
// For now there is one output type only: **version 0 = a 32-byte x-only key**, spent with one
// 64-byte Schnorr signature. That is deliberately little — every further type is surface for bugs
// and one can be added with a soft fork through the reserved versions.
//
// Versions 1–15 are reserved: old nodes treat them today as spendable by anybody (SPEC §2.4).
// That is exactly what allows a new script type to be introduced without a hard fork — but it
// means that **nobody should send funds to such an output until the version is activated**.

const (
	// PubkeyBytes is the length of an x-only public key.
	PubkeyBytes = 32
	// SignatureBytes is the length of a Schnorr signature.
	SignatureBytes = 64
)

var (
	ErrBadPubkeyScript = errors.New("chain: a version 0 output has to contain a 32-byte key")
	ErrMissingWitness  = errors.New("chain: the signature for an input is missing")
	ErrBadTxSignature  = errors.New("chain: invalid transaction signature")
)

// VerifyInput verifies that input `index` may spend `coin`.
func VerifyInput(tx *Tx, index int, coins []Coin) error {
	coin := coins[index]

	switch {
	case coin.ScriptVersion == ScriptVersionBasic:
		if len(coin.Script) != PubkeyBytes {
			return ErrBadPubkeyScript
		}
		if index >= len(tx.Witness) || len(tx.Witness[index].Items) != 1 {
			return ErrMissingWitness
		}
		sigBytes := tx.Witness[index].Items[0]
		if len(sigBytes) != SignatureBytes {
			return fmt.Errorf("%w: the signature has %d B, expected %d", ErrBadTxSignature,
				len(sigBytes), SignatureBytes)
		}
		var pubkey [PubkeyBytes]byte
		var sig [SignatureBytes]byte
		copy(pubkey[:], coin.Script)
		copy(sig[:], sigBytes)

		msg := SigHash(tx, index, coins)
		if !mh.SchnorrVerify(&msg, &pubkey, &sig) {
			return ErrBadTxSignature
		}
		return nil

	case coin.ScriptVersion <= ScriptVersionMaxReserved:
		// A reserved version — spendable by anybody today, so it can be activated with a soft fork.
		return nil

	default:
		// Data outputs never end up in the UTXO set, so this cannot be reached.
		return ErrTxBadVersion
	}
}

// SignInput creates the signature for an input — for the wallet and for tests.
func SignInput(tx *Tx, index int, coins []Coin, seckey *[32]byte) ([]byte, error) {
	msg := SigHash(tx, index, coins)
	sig, err := mh.SchnorrSign(&msg, seckey)
	if err != nil {
		return nil, err
	}
	return sig[:], nil
}
