package p2p

import (
	"encoding/binary"

	"powmachina/internal/chain"
)

// Finality attestations and proofs of double signing travel the same network as blocks.
//
// The format is fixed-length, so it cannot be inflated: an attestation is 105 B, a proof 210 B.

const (
	AttestationBytes  = 8 + 32 + 1 + 64 // the height, the block, the signer index, the signature
	EquivocationBytes = 2 * AttestationBytes
)

func encodeAttestation(a chain.Attestation, out []byte) {
	binary.LittleEndian.PutUint64(out[0:8], a.Height)
	copy(out[8:40], a.BlockID[:])
	out[40] = a.SignerIdx
	copy(out[41:105], a.Signature)
}

func decodeAttestation(raw []byte) (chain.Attestation, error) {
	var a chain.Attestation
	if len(raw) != AttestationBytes {
		return a, ErrBadPayload
	}
	a.Height = binary.LittleEndian.Uint64(raw[0:8])
	copy(a.BlockID[:], raw[8:40])
	a.SignerIdx = raw[40]
	a.Signature = append([]byte(nil), raw[41:105]...)
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
// same signer for different blocks.
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
	// The proof only makes sense when it is the same signer at the same height for different blocks.
	if first.SignerIdx != second.SignerIdx || first.Height != second.Height ||
		first.BlockID == second.BlockID {
		return nil, ErrBadPayload
	}
	return &chain.Equivocation{
		SignerIdx: first.SignerIdx, Height: first.Height, First: first, Second: second,
	}, nil
}
