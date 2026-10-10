package mh

import (
	"errors"

	btcec "github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
)

// Signatures: Schnorr per BIP340 over secp256k1.
//
// They are here for the same reason as the hash — **consensus may have only one implementation**.
// If the node verified signatures in two places with two libraries, any difference in an edge case
// (an invalid point, zero, an overflow) would mean the network falling apart.
//
// Verification is used by the node on every transaction; signing is only needed by the wallet and the
// tests. A signature is deterministic (RFC 6979), so the same input gives the same signature.

// ErrBadKey means an invalid private key — zero or a value above the curve's order.
var ErrBadKey = errors.New("mh: an invalid key")

// privKey verifies that a key is in [1, n-1]. btcec itself would reduce anything modulo n, which would
// turn zero into silent nonsense.
func privKey(seckey *[32]byte) (*btcec.PrivateKey, error) {
	var s btcec.ModNScalar
	if s.SetBytes(seckey) != 0 || s.IsZero() {
		return nil, ErrBadKey
	}
	return btcec.PrivKeyFromScalar(&s), nil
}

// SchnorrVerify verifies a signature over a 32-byte message. Invalid input is always false,
// never a panic.
func SchnorrVerify(msg *[32]byte, pubkey *[32]byte, sig *[64]byte) bool {
	pk, err := schnorr.ParsePubKey(pubkey[:])
	if err != nil {
		return false
	}
	s, err := schnorr.ParseSignature(sig[:])
	if err != nil {
		return false
	}
	return s.Verify(msg[:], pk)
}

// SchnorrSign signs a 32-byte message — for the wallet and the tests, not for the node.
func SchnorrSign(msg *[32]byte, seckey *[32]byte) (*[64]byte, error) {
	sk, err := privKey(seckey)
	if err != nil {
		return nil, err
	}
	sig, err := schnorr.Sign(sk, msg[:])
	if err != nil {
		return nil, err
	}
	var out [64]byte
	copy(out[:], sig.Serialize())
	return &out, nil
}

// SchnorrPubkey derives the x-only public key from a private one.
func SchnorrPubkey(seckey *[32]byte) (*[32]byte, error) {
	sk, err := privKey(seckey)
	if err != nil {
		return nil, err
	}
	var out [32]byte
	copy(out[:], schnorr.SerializePubKey(sk.PubKey()))
	return &out, nil
}
