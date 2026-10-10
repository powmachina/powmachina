// Package address handles the textual form of addresses.
//
// An address is the only part of the system a person copies by hand — which is why it has to have a
// checksum. Without one a typo sends money into nothing and nobody ever gets it back.
//
// We use **bech32m** (BIP350) for every script version. BIP350 distinguishes bech32 for version 0 and
// bech32m for higher ones; we are a new network with no historical baggage, so it is bech32m
// throughout — one fewer rule that can be got wrong.
package address

import (
	"errors"
	"fmt"
	"strings"
)

const charset = "qpzry9x8gf2tvdw0s3jn54khce6mua7l"

// bech32mConst is the checksum constant per BIP350 (bech32 has 1).
const bech32mConst = 0x2bc830a3

// MaxLength is the cap on a whole address's length per BIP173.
const MaxLength = 90

var (
	ErrTooLong     = errors.New("address: the address is longer than 90 characters")
	ErrMixedCase   = errors.New("address: an address must not mix upper and lower case")
	ErrNoSeparator = errors.New("address: the separator '1' is missing")
	ErrBadHRP      = errors.New("address: the wrong network prefix")
	ErrBadChar     = errors.New("address: an invalid character")
	ErrBadChecksum = errors.New("address: the checksum does not match — probably a typo")
	ErrBadData     = errors.New("address: invalid address data")
)

var charsetRev = func() [128]int8 {
	var rev [128]int8
	for i := range rev {
		rev[i] = -1
	}
	for i, c := range charset {
		rev[c] = int8(i)
	}
	return rev
}()

func polymod(values []byte) uint32 {
	gen := [5]uint32{0x3b6a57b2, 0x26508e6d, 0x1ea119fa, 0x3d4233dd, 0x2a1462b3}
	chk := uint32(1)
	for _, v := range values {
		b := chk >> 25
		chk = (chk&0x1ffffff)<<5 ^ uint32(v)
		for i := 0; i < 5; i++ {
			if (b>>uint(i))&1 == 1 {
				chk ^= gen[i]
			}
		}
	}
	return chk
}

func hrpExpand(hrp string) []byte {
	out := make([]byte, 0, len(hrp)*2+1)
	for i := 0; i < len(hrp); i++ {
		out = append(out, hrp[i]>>5)
	}
	out = append(out, 0)
	for i := 0; i < len(hrp); i++ {
		out = append(out, hrp[i]&31)
	}
	return out
}

// EncodeBech32m encodes 5-bit data with a prefix.
func EncodeBech32m(hrp string, data []byte) (string, error) {
	if len(hrp)+len(data)+7 > MaxLength {
		return "", ErrTooLong
	}
	if len(hrp) == 0 {
		return "", ErrBadHRP
	}
	values := append(hrpExpand(hrp), data...)
	values = append(values, 0, 0, 0, 0, 0, 0)
	mod := polymod(values) ^ bech32mConst

	var sb strings.Builder
	sb.WriteString(hrp)
	sb.WriteByte('1')
	for _, d := range data {
		if d >= 32 {
			return "", ErrBadData
		}
		sb.WriteByte(charset[d])
	}
	for i := 0; i < 6; i++ {
		sb.WriteByte(charset[(mod>>uint(5*(5-i)))&31])
	}
	return sb.String(), nil
}

// DecodeBech32m decodes an address into its prefix and 5-bit data.
func DecodeBech32m(s string) (string, []byte, error) {
	if len(s) > MaxLength {
		return "", nil, ErrTooLong
	}
	lower, upper := strings.ToLower(s), strings.ToUpper(s)
	if s != lower && s != upper {
		return "", nil, ErrMixedCase
	}
	s = lower

	pos := strings.LastIndexByte(s, '1')
	if pos < 1 || pos+7 > len(s) {
		return "", nil, ErrNoSeparator
	}
	hrp := s[:pos]
	for i := 0; i < len(hrp); i++ {
		if hrp[i] < 33 || hrp[i] > 126 {
			return "", nil, ErrBadHRP
		}
	}

	data := make([]byte, 0, len(s)-pos-1)
	for i := pos + 1; i < len(s); i++ {
		c := s[i]
		if c >= 128 || charsetRev[c] == -1 {
			return "", nil, ErrBadChar
		}
		data = append(data, byte(charsetRev[c]))
	}
	if polymod(append(hrpExpand(hrp), data...)) != bech32mConst {
		return "", nil, ErrBadChecksum
	}
	return hrp, data[:len(data)-6], nil
}

// convertBits converts between 8-bit and 5-bit groups.
func convertBits(data []byte, from, to uint8, pad bool) ([]byte, error) {
	var acc, bits uint32
	maxv := uint32(1<<to) - 1
	out := make([]byte, 0, len(data)*int(from)/int(to)+1)
	for _, b := range data {
		if from == 8 && b > 255 {
			return nil, ErrBadData
		}
		acc = acc<<from | uint32(b)
		bits += uint32(from)
		for bits >= uint32(to) {
			bits -= uint32(to)
			out = append(out, byte(acc>>bits&maxv))
		}
	}
	if pad {
		if bits > 0 {
			out = append(out, byte(acc<<(uint32(to)-bits)&maxv))
		}
	} else if bits >= uint32(from) || byte(acc<<(uint32(to)-bits)&maxv) != 0 {
		return nil, ErrBadData // non-zero padding = the address has more than one form
	}
	return out, nil
}

// Encode assembles an address from a script version and a program (a 32-byte key in our case).
func Encode(hrp string, scriptVersion byte, program []byte) (string, error) {
	if scriptVersion > 15 {
		return "", fmt.Errorf("%w: version %d", ErrBadData, scriptVersion)
	}
	if len(program) < 2 || len(program) > 40 {
		return "", fmt.Errorf("%w: the program has %d bytes", ErrBadData, len(program))
	}
	converted, err := convertBits(program, 8, 5, true)
	if err != nil {
		return "", err
	}
	return EncodeBech32m(hrp, append([]byte{scriptVersion}, converted...))
}

// Decode decodes an address and verifies that it belongs to the given network.
func Decode(expectedHRP, addr string) (scriptVersion byte, program []byte, err error) {
	hrp, data, err := DecodeBech32m(addr)
	if err != nil {
		return 0, nil, err
	}
	if hrp != expectedHRP {
		return 0, nil, fmt.Errorf("%w: the address is for the network %q, not %q", ErrBadHRP, hrp, expectedHRP)
	}
	if len(data) < 1 {
		return 0, nil, ErrBadData
	}
	version := data[0]
	if version > 15 {
		return 0, nil, fmt.Errorf("%w: version %d", ErrBadData, version)
	}
	program, err = convertBits(data[1:], 5, 8, false)
	if err != nil {
		return 0, nil, err
	}
	if len(program) < 2 || len(program) > 40 {
		return 0, nil, fmt.Errorf("%w: the program has %d bytes", ErrBadData, len(program))
	}
	return version, program, nil
}
