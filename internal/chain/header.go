package chain

import (
	"encoding/binary"
	"errors"
	"time"

	"powmachina/internal/mh"
)

// PoWBytes is the part of the header MachinaHash is computed over. The nonce and the lane do not
// belong in it — they go into the hash separately, so a nonce found in one lane is useless in another.
const PoWBytes = mh.HeaderBytes // 80

// HeaderBytes is the header as it goes over the network: the PoW part + the nonce + the lane.
const HeaderBytes = PoWBytes + 8 + 1 // 89

// Header is a block header.
//
// Height is part of the header on purpose: it determines the epoch — lane G's DAG and lane C's
// RandomX key — so it cannot be swapped without changing the PoW.
type Header struct {
	Version    uint32
	PrevBlock  [32]byte
	MerkleRoot [32]byte
	Time       uint32
	Bits       uint32
	Height     uint32
	Nonce      uint64
	Lane       mh.Lane
}

var ErrShortHeader = errors.New("chain: the header is shorter than 89 B")
var ErrBadLane = errors.New("chain: unknown lane")

// PoWInput returns the 80 B the PoW is computed over.
func (h *Header) PoWInput() [PoWBytes]byte {
	var b [PoWBytes]byte
	binary.LittleEndian.PutUint32(b[0:4], h.Version)
	copy(b[4:36], h.PrevBlock[:])
	copy(b[36:68], h.MerkleRoot[:])
	binary.LittleEndian.PutUint32(b[68:72], h.Time)
	binary.LittleEndian.PutUint32(b[72:76], h.Bits)
	binary.LittleEndian.PutUint32(b[76:80], h.Height)
	return b
}

// Serialize returns the header for the network and for computing the block ID.
func (h *Header) Serialize() [HeaderBytes]byte {
	var out [HeaderBytes]byte
	pow := h.PoWInput()
	copy(out[:PoWBytes], pow[:])
	binary.LittleEndian.PutUint64(out[PoWBytes:PoWBytes+8], h.Nonce)
	out[PoWBytes+8] = byte(h.Lane)
	return out
}

// DeserializeHeader reads a header from the network.
func DeserializeHeader(b []byte) (*Header, error) {
	if len(b) < HeaderBytes {
		return nil, ErrShortHeader
	}
	h := &Header{
		Version: binary.LittleEndian.Uint32(b[0:4]),
		Time:    binary.LittleEndian.Uint32(b[68:72]),
		Bits:    binary.LittleEndian.Uint32(b[72:76]),
		Height:  binary.LittleEndian.Uint32(b[76:80]),
		Nonce:   binary.LittleEndian.Uint64(b[80:88]),
		Lane:    mh.Lane(b[88]),
	}
	copy(h.PrevBlock[:], b[4:36])
	copy(h.MerkleRoot[:], b[36:68])
	if h.Lane > mh.LaneG {
		return nil, ErrBadLane
	}
	return h, nil
}

// ID is the hash of the header. The definition lives in internal/mh — one implementation for the whole project.
func (h *Header) ID() [32]byte {
	s := h.Serialize()
	return mh.BlockID(s[:])
}

func (h *Header) Timestamp() time.Time { return time.Unix(int64(h.Time), 0).UTC() }

// CheckPoW verifies that a header meets its own target. The caller has to have the verifier for the
// epoch matching the header's height.
func (h *Header) CheckPoW(hasher *mh.Hasher, v *mh.Verifier) (bool, error) {
	if h.Lane > mh.LaneG {
		return false, ErrBadLane
	}
	if want := EpochForHeight(uint64(h.Height)); v.Epoch() != want {
		return false, errors.New("chain: the verifier has a different epoch than the header needs")
	}
	pow := h.PoWInput()
	got, err := hasher.Hash(v, &pow, h.Nonce, h.Lane)
	if err != nil {
		return false, err
	}
	target, err := CompactToTarget(h.Bits)
	if err != nil {
		return false, err
	}
	return mh.MeetsTarget(&got, &target), nil
}
