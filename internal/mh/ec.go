package mh

import (
	"errors"

	btcec "github.com/btcsuite/btcd/btcec/v2"
)

// The operations over secp256k1 that BIP32 needs. They are here for the same reason as the signatures
// — curve arithmetic must not have two implementations in the project.

// ErrSkipIndex means no key exists for this index. BIP32 says it should be skipped and the next one
// tried.
var ErrSkipIndex = errors.New("mh: no key exists for this index, the next one is needed")

// CompressedPubkey returns the 33-byte SEC1 public key. BIP32 needs it when deriving non-hardened
// children, where the parent's public key goes into the HMAC.
func CompressedPubkey(seckey *[32]byte) (*[33]byte, error) {
	sk, err := privKey(seckey)
	if err != nil {
		return nil, err
	}
	var out [33]byte
	copy(out[:], sk.PubKey().SerializeCompressed())
	return &out, nil
}

// ScalarAdd computes (a + b) mod n.
//
// It returns an error if either input is above the curve's order or the result is zero — such a key
// does not exist and per BIP32 the index should be skipped.
func ScalarAdd(a, b *[32]byte) (*[32]byte, error) {
	var sa, sb btcec.ModNScalar
	if sa.SetBytes(a) != 0 {
		return nil, ErrSkipIndex
	}
	if sb.SetBytes(b) != 0 {
		return nil, ErrSkipIndex
	}
	var sum btcec.ModNScalar
	sum.Add2(&sa, &sb)
	if sum.IsZero() {
		return nil, ErrSkipIndex
	}
	out := sum.Bytes()
	return &out, nil
}
