// Package hd derives keys from one seed per BIP32.
//
// What it is for: without it a wallet is a list of random keys and a backup has to be made after every
// new address. With a seed the backup is made **once** and every address can be restored from it.
//
// What is **not** here yet: the BIP39 mnemonic (24 words). The reason is concrete — it requires the
// exact list of 2048 words and that must not be written from memory; it has to be taken over and
// verified against a known hash. The seed is therefore shown as hex for now. See the security notes.
package hd

import (
	"crypto/hmac"
	"crypto/sha512"
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"powmachina/internal/mh"
)

// HardenedOffset is the boundary of hardened indices (BIP32).
const HardenedOffset uint32 = 0x80000000

// masterKeyTag is the HMAC key for deriving the master key. The value comes from BIP32 and **must not
// change** — otherwise a seed would give different addresses in another wallet.
var masterKeyTag = []byte("Bitcoin seed")

var (
	ErrShortSeed  = errors.New("hd: the seed has to be at least 16 bytes")
	ErrBadPath    = errors.New("hd: an invalid derivation path")
	ErrPublicOnly = errors.New("hd: nothing can be derived without the private key")
)

// Key is a node in the derivation tree.
type Key struct {
	Priv        [32]byte
	ChainCode   [32]byte
	Depth       uint8
	ChildNumber uint32
}

// NewMaster derives the master key from a seed.
func NewMaster(seed []byte) (*Key, error) {
	if len(seed) < 16 {
		return nil, ErrShortSeed
	}
	mac := hmac.New(sha512.New, masterKeyTag)
	mac.Write(seed)
	sum := mac.Sum(nil)

	k := &Key{}
	copy(k.Priv[:], sum[:32])
	copy(k.ChainCode[:], sum[32:])
	// The master key has to be a valid scalar; if it is not, the seed cannot be used.
	if _, err := mh.CompressedPubkey(&k.Priv); err != nil {
		return nil, fmt.Errorf("hd: the seed gave an invalid key: %w", err)
	}
	return k, nil
}

// Child derives a child. An index above HardenedOffset means hardened derivation.
//
// With hardened derivation the private key goes into the HMAC, with non-hardened the public one — that is
// the whole difference and the reason a watch-only wallet can be built from a non-hardened branch.
func (k *Key) Child(index uint32) (*Key, error) {
	data := make([]byte, 0, 37)
	if index >= HardenedOffset {
		data = append(data, 0x00)
		data = append(data, k.Priv[:]...)
	} else {
		pub, err := mh.CompressedPubkey(&k.Priv)
		if err != nil {
			return nil, err
		}
		data = append(data, pub[:]...)
	}
	var idx [4]byte
	binary.BigEndian.PutUint32(idx[:], index)
	data = append(data, idx[:]...)

	mac := hmac.New(sha512.New, k.ChainCode[:])
	mac.Write(data)
	sum := mac.Sum(nil)

	var tweak [32]byte
	copy(tweak[:], sum[:32])
	priv, err := mh.ScalarAdd(&k.Priv, &tweak)
	if err != nil {
		return nil, err // per BIP32 such an index is skipped
	}

	child := &Key{Depth: k.Depth + 1, ChildNumber: index, Priv: *priv}
	copy(child.ChainCode[:], sum[32:])
	return child, nil
}

// Derive walks a path in the form `m/44'/9797'/0'/0/5`. An apostrophe means a hardened index.
func (k *Key) Derive(path string) (*Key, error) {
	parts := strings.Split(strings.TrimSpace(path), "/")
	if len(parts) == 0 || (parts[0] != "m" && parts[0] != "M") {
		return nil, ErrBadPath
	}
	cur := k
	for _, p := range parts[1:] {
		if p == "" {
			return nil, ErrBadPath
		}
		hardened := false
		if strings.HasSuffix(p, "'") || strings.HasSuffix(p, "h") || strings.HasSuffix(p, "H") {
			hardened = true
			p = p[:len(p)-1]
		}
		n, err := strconv.ParseUint(p, 10, 32)
		if err != nil || n >= uint64(HardenedOffset) {
			return nil, fmt.Errorf("%w: %q", ErrBadPath, p)
		}
		index := uint32(n)
		if hardened {
			index += HardenedOffset
		}
		cur, err = cur.Child(index)
		if err != nil {
			return nil, err
		}
	}
	return cur, nil
}

// XOnlyPubkey returns the 32-byte key that goes into an address.
func (k *Key) XOnlyPubkey() ([32]byte, error) {
	pk, err := mh.SchnorrPubkey(&k.Priv)
	if err != nil {
		return [32]byte{}, err
	}
	return *pk, nil
}

// PrivateKey returns the private key for signing.
func (k *Key) PrivateKey() *[32]byte {
	out := k.Priv
	return &out
}
