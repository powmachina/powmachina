package p2p

import (
	"encoding/binary"

	"powmachina/internal/chain"
)

// Finality attestations and proofs of double signing travel the same network as blocks.
//
// The format is fixed-length, so it cannot be inflated: an attestation is 136 B, a proof 272 B.

const (
	AttestationBytes  = 8 + 32 + 32 + 64 // the height, the block, the signer's key, the signature
	EquivocationBytes = 2 * AttestationBytes
)

func encodeAttestation(a chain.Attestation, out []byte) {
	binary.LittleEndian.PutUint64(out[0:8], a.Height)
	copy(out[8:40], a.BlockID[:])
	copy(out[40:72], a.Key[:])
	copy(out[72:136], a.Signature)
}

func decodeAttestation(raw []byte) (chain.Attestation, error) {
	var a chain.Attestation
	if len(raw) != AttestationBytes {
		return a, ErrBadPayload
	}
	a.Height = binary.LittleEndian.Uint64(raw[0:8])
	copy(a.BlockID[:], raw[8:40])
	copy(a.Key[:], raw[40:72])
	a.Signature = append([]byte(nil), raw[72:136]...)
	return a, nil
}

// EncodeAttestation packs an attestation.
func EncodeAttestation(a chain.Attestation) ([]byte, error) {
	if len(a.Signature) != 64 {
		return nil, ErrBadPayload
	}
	out := make([]byte, AttestationBytes)
	encodeAttestation(a, out)
	return out, nil
}

func DecodeAttestation(payload []byte) (chain.Attestation, error) {
	return decodeAttestation(payload)
}

// EncodeEquivocation packs proof of double signing — two attestations of the same height from the
// same key for different blocks.
func EncodeEquivocation(e *chain.Equivocation) ([]byte, error) {
	if len(e.First.Signature) != 64 || len(e.Second.Signature) != 64 {
		return nil, ErrBadPayload
	}
	out := make([]byte, EquivocationBytes)
	encodeAttestation(e.First, out[:AttestationBytes])
	encodeAttestation(e.Second, out[AttestationBytes:])
	return out, nil
}

func DecodeEquivocation(payload []byte) (*chain.Equivocation, error) {
	if len(payload) != EquivocationBytes {
		return nil, ErrBadPayload
	}
	first, err := decodeAttestation(payload[:AttestationBytes])
	if err != nil {
		return nil, err
	}
	second, err := decodeAttestation(payload[AttestationBytes:])
	if err != nil {
		return nil, err
	}
	// The proof only makes sense when it is the same key at the same height for different blocks.
	if first.Key != second.Key || first.Height != second.Height || first.BlockID == second.BlockID {
		return nil, ErrBadPayload
	}
	return &chain.Equivocation{Key: first.Key, Height: first.Height, First: first, Second: second}, nil
}
