package chain

import "powmachina/internal/mh"

// A merkle tree over txids.
//
// Two things are deliberately different here from Bitcoin:
//
//  1. Leaves and internal nodes have **different domains**. Without that an internal node can be
//     passed off as a leaf.
//  2. With an odd count the last node is **carried up unchanged**, never duplicated. Duplication
//     is the cause of CVE-2012-2459 in Bitcoin: two different transaction lists lead to the same
//     root, which lets a valid block be declared invalid.
const (
	merkleLeafTag = "PowMachina/merkle-leaf/v0"
	merkleNodeTag = "PowMachina/merkle-node/v0"
)

// MerkleRoot computes the root over a list of IDs. An empty list gives a zero root.
func MerkleRoot(ids [][32]byte) [32]byte {
	if len(ids) == 0 {
		return [32]byte{}
	}
	level := make([][32]byte, len(ids))
	for i, id := range ids {
		level[i] = mh.TaggedHash(merkleLeafTag, id[:])
	}
	for len(level) > 1 {
		next := make([][32]byte, 0, (len(level)+1)/2)
		for i := 0; i < len(level); i += 2 {
			if i+1 == len(level) {
				// An odd node goes up unchanged — no duplication.
				next = append(next, level[i])
				continue
			}
			var pair [64]byte
			copy(pair[:32], level[i][:])
			copy(pair[32:], level[i+1][:])
			next = append(next, mh.TaggedHash(merkleNodeTag, pair[:]))
		}
		level = next
	}
	return level[0]
}

// TxMerkleRoot computes the root over a block's transactions.
func TxMerkleRoot(txs []*Tx) [32]byte {
	ids := make([][32]byte, len(txs))
	for i, t := range txs {
		ids[i] = t.TxID()
	}
	return MerkleRoot(ids)
}

// WitnessMerkleRoot covers the signatures; the commitment to it sits in the coinbase (witness.go).
//
// The coinbase's own leaf is all zeros: the commitment is one of its outputs, so its id cannot
// be part of what the commitment covers. Nothing is lost by that — a coinbase has no witness.
func WitnessMerkleRoot(txs []*Tx) [32]byte {
	ids := make([][32]byte, len(txs))
	for i, t := range txs {
		if i == 0 {
			continue
		}
		ids[i] = t.WTxID()
	}
	return MerkleRoot(ids)
}
