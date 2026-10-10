package p2p

import (
	"encoding/binary"
	"errors"

	"powmachina/internal/chain"
)

// The messages for header synchronization.
//
// The item counts are read first and **checked before any allocation** — otherwise a `headers`
// message with the number four billion is the cheapest way to take a node down.

// MaxLocatorEntries is the cap on a locator's length. A locator grows exponentially, so 32 entries
// cover even a chain of a million blocks.
const MaxLocatorEntries = 32

var (
	ErrShortPayload = errors.New("p2p: the payload is shorter than the item count it reports")
	ErrBadPayload   = errors.New("p2p: the payload has the wrong length")
)

// EncodeGetHeaders packs a locator.
func EncodeGetHeaders(locator [][32]byte) ([]byte, error) {
	if err := CheckItemCount(len(locator), MaxLocatorEntries); err != nil {
		return nil, err
	}
	out := make([]byte, 2+len(locator)*32)
	binary.LittleEndian.PutUint16(out[0:2], uint16(len(locator)))
	for i, id := range locator {
		copy(out[2+i*32:], id[:])
	}
	return out, nil
}

func DecodeGetHeaders(payload []byte) ([][32]byte, error) {
	if len(payload) < 2 {
		return nil, ErrShortPayload
	}
	n := int(binary.LittleEndian.Uint16(payload[0:2]))
	if err := CheckItemCount(n, MaxLocatorEntries); err != nil {
		return nil, err
	}
	if len(payload) != 2+n*32 {
		return nil, ErrBadPayload
	}
	out := make([][32]byte, n)
	for i := range out {
		copy(out[i][:], payload[2+i*32:2+(i+1)*32])
	}
	return out, nil
}

// EncodeHeaders packs headers. Each one is a fixed 89 B, so no lengths are needed.
func EncodeHeaders(headers []*chain.Header) ([]byte, error) {
	if err := CheckItemCount(len(headers), MaxHeadersPerMessage); err != nil {
		return nil, err
	}
	out := make([]byte, 2+len(headers)*chain.HeaderBytes)
	binary.LittleEndian.PutUint16(out[0:2], uint16(len(headers)))
	for i, h := range headers {
		raw := h.Serialize()
		copy(out[2+i*chain.HeaderBytes:], raw[:])
	}
	return out, nil
}

func DecodeHeaders(payload []byte) ([]*chain.Header, error) {
	if len(payload) < 2 {
		return nil, ErrShortPayload
	}
	n := int(binary.LittleEndian.Uint16(payload[0:2]))
	if err := CheckItemCount(n, MaxHeadersPerMessage); err != nil {
		return nil, err
	}
	if len(payload) != 2+n*chain.HeaderBytes {
		return nil, ErrBadPayload
	}
	out := make([]*chain.Header, 0, n)
	for i := 0; i < n; i++ {
		off := 2 + i*chain.HeaderBytes
		h, err := chain.DeserializeHeader(payload[off : off+chain.HeaderBytes])
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, nil
}
