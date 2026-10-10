package p2p

import (
	"encoding/binary"

	"powmachina/internal/chain"
)

// The request for finality attestations and the answer to it.
//
// # What it is for
//
// Finality state lives only in the node's memory and duplicate attestations are **deliberately not
// relayed** (otherwise gossip never ends — see `chain.FinalityState.Add`). A node that restarted
// therefore has no way to learn about the existing attestations: the peers have already forwarded
// them and will not do it again. Without this message its finality lags until the quorum signs the
// next height — with a depth of 20 blocks, up to 40 minutes. So they can be requested.
//
// The answer has fixed caps (`MaxAttestHeights`, `MaxAttestPerMessage`), so a request cannot force a
// large message.

const getAttestBytes = 8 + 2 // from height, number of heights

// EncodeGetAttest packs a request for the attestations of heights `from` … `from+count-1`.
func EncodeGetAttest(from uint64, count uint16) ([]byte, error) {
	if count == 0 || int(count) > MaxAttestHeights {
		return nil, ErrBadPayload
	}
	out := make([]byte, getAttestBytes)
	binary.LittleEndian.PutUint64(out[0:8], from)
	binary.LittleEndian.PutUint16(out[8:10], count)
	return out, nil
}

// DecodeGetAttest reads a request from a foreign peer — that is, untrusted bytes.
func DecodeGetAttest(payload []byte) (from uint64, count uint16, err error) {
	if len(payload) != getAttestBytes {
		return 0, 0, ErrBadPayload
	}
	from = binary.LittleEndian.Uint64(payload[0:8])
	count = binary.LittleEndian.Uint16(payload[8:10])
	if count == 0 || int(count) > MaxAttestHeights {
		return 0, 0, ErrTooManyItems
	}
	return from, count, nil
}

// EncodeAttestations packs the answer. An empty list is a valid answer ("I have nothing").
func EncodeAttestations(list []chain.Attestation) ([]byte, error) {
	if len(list) > MaxAttestPerMessage {
		return nil, ErrTooManyItems
	}
	out := make([]byte, 2+len(list)*AttestationBytes)
	binary.LittleEndian.PutUint16(out[0:2], uint16(len(list)))
	for i, a := range list {
		if len(a.Signature) != 64 {
			return nil, ErrBadPayload
		}
		encodeAttestation(a, out[2+i*AttestationBytes:2+(i+1)*AttestationBytes])
	}
	return out, nil
}

func DecodeAttestations(payload []byte) ([]chain.Attestation, error) {
	if len(payload) < 2 {
		return nil, ErrBadPayload
	}
	n := int(binary.LittleEndian.Uint16(payload[0:2]))
	if n > MaxAttestPerMessage {
		return nil, ErrTooManyItems
	}
	if len(payload) != 2+n*AttestationBytes {
		return nil, ErrBadPayload
	}
	out := make([]chain.Attestation, 0, n)
	for i := 0; i < n; i++ {
		a, err := decodeAttestation(payload[2+i*AttestationBytes : 2+(i+1)*AttestationBytes])
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, nil
}
