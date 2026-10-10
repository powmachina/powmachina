package address

import (
	"errors"
	"fmt"
)

// A private key for export and import: bech32m with the network prefix + "sec" and a 32-byte key.
//
//	mainnet powmsec1…   testnet tpowmsec1…   regtest rpowmsec1…
//
// Why not hex: the checksum catches a typo while copying it over, and the prefix makes it immediately
// clear that this is a secret and which network it belongs to. It cannot be confused with an address
// (`powm1…`) — Decode with the address prefix rejects it and the other way round.

// SecretSuffix is appended to the network's address prefix.
const SecretSuffix = "sec"

// secretVersion is the first 5-bit symbol — room for another key type in the future.
const secretVersion = 0

var ErrNotSecret = errors.New("address: not a private key for this network")

// EncodeSecret encodes a 32-byte private key for the network whose address prefix is `hrp`.
func EncodeSecret(hrp string, key *[32]byte) (string, error) {
	return Encode(hrp+SecretSuffix, secretVersion, key[:])
}

// DecodeSecret decodes a private key and verifies both the network and the length.
func DecodeSecret(hrp, s string) (*[32]byte, error) {
	version, program, err := Decode(hrp+SecretSuffix, s)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrNotSecret, err)
	}
	if version != secretVersion || len(program) != 32 {
		return nil, ErrNotSecret
	}
	var k [32]byte
	copy(k[:], program)
	return &k, nil
}
