// Package bip39 is the BIP39 mnemonic: 24 words from the official English list that carry 256 bits of
// randomness and an 8-bit checksum.
//
// Why a standard and not our own words: people know BIP39, hardware wallets support it and the list is
// designed so that the first 4 letters of every word are unique (a typo can be corrected, a word can be
// written abbreviated). The `english.txt` list is verified against the SHA-256 from BIP39
// (TestWordlistMatchesBIP39), the implementation against Trezor's vectors.
//
// The difference from BIP39: the passphrase has to be ASCII. BIP39 normalizes it through Unicode NFKD —
// otherwise an accented letter written in two ways would give two different wallets. We do not write the
// normalization ourselves; instead we refuse a non-ASCII passphrase before it turns into lost money.
package bip39

import (
	"crypto/pbkdf2"
	"crypto/sha256"
	"crypto/sha512"
	_ "embed"
	"errors"
	"fmt"
	"strings"
)

//go:embed english.txt
var englishTxt string

var (
	words []string
	index map[string]int
	// prefix: the first 4 letters → the index (in the BIP39 list they are unique)
	prefix map[string]int
)

func init() {
	words = strings.Fields(englishTxt)
	if len(words) != 2048 {
		panic("bip39: the word list does not have 2048 entries")
	}
	index = make(map[string]int, 2048)
	prefix = make(map[string]int, 2048)
	for i, w := range words {
		index[w] = i
		p := w
		if len(p) > 4 {
			p = p[:4]
		}
		if _, dup := prefix[p]; dup {
			panic("bip39: the prefix " + p + " is not unique")
		}
		prefix[p] = i
	}
}

var (
	ErrEntropy      = errors.New("bip39: entropy must be 16–32 bytes in steps of 4")
	ErrWordCount    = errors.New("bip39: the phrase must have 12, 15, 18, 21 or 24 words")
	ErrChecksum     = errors.New("bip39: checksum mismatch — a word is wrong or words are swapped")
	ErrNonASCIIPass = errors.New("bip39: the passphrase must be ASCII")
)

// NewMnemonic encodes randomness into words.
func NewMnemonic(entropy []byte) (string, error) {
	n := len(entropy)
	if n < 16 || n > 32 || n%4 != 0 {
		return "", ErrEntropy
	}
	sum := sha256.Sum256(entropy)
	csBits := n * 8 / 32
	bits := append(append([]byte{}, entropy...), sum[0])
	count := (n*8 + csBits) / 11
	out := make([]string, count)
	for i := range count {
		out[i] = words[take11(bits, i*11)]
	}
	return strings.Join(out, " "), nil
}

// take11 reads 11 bits from the position `pos` (big-endian, as in BIP39).
func take11(b []byte, pos int) int {
	v := 0
	for i := range 11 {
		bit := pos + i
		v = v<<1 | int(b[bit/8]>>(7-bit%8)&1)
	}
	return v
}

// Normalize turns input from a person into the canonical mnemonic: lower case, single spaces, words
// given by their first four letters completed. An unknown word is an error with its position.
func Normalize(input string) (string, error) {
	fields := strings.Fields(strings.ToLower(input))
	for i, f := range fields {
		if _, ok := index[f]; ok {
			continue
		}
		if len(f) >= 4 {
			if j, ok := prefix[f[:4]]; ok && strings.HasPrefix(words[j], f) {
				fields[i] = words[j]
				continue
			}
		}
		return "", fmt.Errorf("bip39: word %d %q is not in the word list", i+1, f)
	}
	return strings.Join(fields, " "), nil
}

// Entropy verifies a mnemonic (the words and the checksum) and returns the randomness it carries.
func Entropy(mnemonic string) ([]byte, error) {
	m, err := Normalize(mnemonic)
	if err != nil {
		return nil, err
	}
	ws := strings.Fields(m)
	switch len(ws) {
	case 12, 15, 18, 21, 24:
	default:
		return nil, ErrWordCount
	}
	totalBits := len(ws) * 11
	csBits := totalBits / 33
	entBytes := (totalBits - csBits) / 8
	buf := make([]byte, (totalBits+7)/8)
	for i, w := range ws {
		v := index[w]
		for j := range 11 {
			if v>>(10-j)&1 == 1 {
				bit := i*11 + j
				buf[bit/8] |= 1 << (7 - bit%8)
			}
		}
	}
	entropy := buf[:entBytes]
	sum := sha256.Sum256(entropy)
	if buf[entBytes]>>(8-csBits) != sum[0]>>(8-csBits) {
		return nil, ErrChecksum
	}
	return append([]byte{}, entropy...), nil
}

// Seed derives the 64-byte seed for BIP32: PBKDF2-HMAC-SHA512, 2048 iterations, the salt "mnemonic" +
// the passphrase. It verifies the mnemonic first — a typo must not silently give a different wallet.
func Seed(mnemonic, passphrase string) ([]byte, error) {
	m, err := Normalize(mnemonic)
	if err != nil {
		return nil, err
	}
	if _, err := Entropy(m); err != nil {
		return nil, err
	}
	for i := 0; i < len(passphrase); i++ {
		if passphrase[i] >= 0x80 {
			return nil, ErrNonASCIIPass
		}
	}
	return pbkdf2.Key(sha512.New, m, []byte("mnemonic"+passphrase), 2048, 64)
}
