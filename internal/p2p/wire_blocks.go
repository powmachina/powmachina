package p2p

import (
	"encoding/binary"

	"powmachina/internal/chain"
)

// The messages that carry block bodies.
//
// A node first downloads the headers (cheap, verifiable) and only then asks for the bodies of the
// blocks that really belong to its chain. The opposite order would let a peer send megabytes of data
// the node does not even want.

// MaxGetDataItems keeps one request at a reasonable size.
const MaxGetDataItems = 128

// The types of inventory items. Without a type there would be no way to tell whether an ID belongs
// to a block or a transaction, and a peer could force us to look in the wrong table.
const (
	InvBlock uint8 = 1
	InvTx    uint8 = 2
)

// InvItem is one item of an offer or a request.
type InvItem struct {
	Type uint8
	ID   [32]byte
}

func encodeItems(items []InvItem, max int) ([]byte, error) {
	if err := CheckItemCount(len(items), max); err != nil {
		return nil, err
	}
	out := make([]byte, 2+len(items)*33)
	binary.LittleEndian.PutUint16(out[0:2], uint16(len(items)))
	for i, it := range items {
		out[2+i*33] = it.Type
		copy(out[3+i*33:], it.ID[:])
	}
	return out, nil
}

func decodeItems(payload []byte, max int) ([]InvItem, error) {
	if len(payload) < 2 {
		return nil, ErrShortPayload
	}
	n := int(binary.LittleEndian.Uint16(payload[0:2]))
	if err := CheckItemCount(n, max); err != nil {
		return nil, err
	}
	if len(payload) != 2+n*33 {
		return nil, ErrBadPayload
	}
	out := make([]InvItem, n)
	for i := range out {
		t := payload[2+i*33]
		if t != InvBlock && t != InvTx {
			return nil, ErrBadPayload // an unknown type = it cannot be processed unambiguously
		}
		out[i].Type = t
		copy(out[i].ID[:], payload[3+i*33:3+i*33+32])
	}
	return out, nil
}

// EncodeInv packs an offer of what we have.
func EncodeInv(items []InvItem) ([]byte, error) { return encodeItems(items, MaxInvPerMessage) }

// DecodeInv reads an offer from a peer.
func DecodeInv(payload []byte) ([]InvItem, error) { return decodeItems(payload, MaxInvPerMessage) }

// EncodeGetData packs a request for particular blocks or transactions.
func EncodeGetData(items []InvItem) ([]byte, error) { return encodeItems(items, MaxGetDataItems) }

func DecodeGetData(payload []byte) ([]InvItem, error) { return decodeItems(payload, MaxGetDataItems) }

// BlockItems is a helper for building a request for blocks.
func BlockItems(ids [][32]byte) []InvItem {
	out := make([]InvItem, len(ids))
	for i, id := range ids {
		out[i] = InvItem{Type: InvBlock, ID: id}
	}
	return out
}

// TxItems is a helper for offering transactions.
func TxItems(ids [][32]byte) []InvItem {
	out := make([]InvItem, len(ids))
	for i, id := range ids {
		out[i] = InvItem{Type: InvTx, ID: id}
	}
	return out
}

// EncodeBlock packs one block. One larger than MaxMessageBytes is not sent.
func EncodeBlock(b *chain.Block) ([]byte, error) {
	raw := b.Serialize()
	if len(raw) > MaxMessageBytes {
		return nil, ErrTooLarge
	}
	return raw, nil
}

func DecodeBlock(payload []byte) (*chain.Block, error) {
	return chain.DeserializeBlock(payload)
}

// EncodeTx and DecodeTx carry one transaction.
func EncodeTx(tx *chain.Tx) ([]byte, error) {
	raw := tx.Serialize()
	if len(raw) > MaxMessageBytes {
		return nil, ErrTooLarge
	}
	return raw, nil
}

func DecodeTx(payload []byte) (*chain.Tx, error) {
	tx, used, err := chain.DeserializeTx(payload)
	if err != nil {
		return nil, err
	}
	if used != len(payload) {
		return nil, ErrBadPayload // leftover bytes = more than one form of the same transaction
	}
	return tx, nil
}
