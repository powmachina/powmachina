// Package wallettest holds a fake node for the wallet and wallet-rpc tests.
package wallettest

import (
	"errors"

	"powmachina/internal/chain"
)

// Node is an in-memory chain. The blocks are not valid from consensus's point of view — the wallet does
// not verify consensus, the node does that.
type Node struct {
	Chain  []*chain.Block
	Pool   map[[32]byte]*chain.Tx
	Reject bool // Broadcast fails (the inputs are already spent elsewhere)
	Busy   bool // Broadcast fails for now (the mempool is full)
	// LoseReply: Broadcast takes the transaction and then fails the way a lost answer does.
	LoseReply bool
	// Silent: Broadcast fails the way a timeout does, and the transaction never arrived.
	Silent bool
}

// refusal is the node's own "no", as opposed to an answer that never came.
type refusal struct {
	msg       string
	temporary bool
}

func (r *refusal) Error() string   { return r.msg }
func (r *refusal) Rejected() bool  { return true }
func (r *refusal) Temporary() bool { return r.temporary }

// NewNode creates a chain with a genesis block.
func NewNode() *Node {
	n := &Node{Pool: make(map[[32]byte]*chain.Tx)}
	n.Mine(nil, nil) // genesis
	return n
}

// Mine adds a block with a coinbase to `pay` (or to nobody) and the transactions from the mempool + `extra`.
func (n *Node) Mine(pay *[32]byte, extra []*chain.Tx) *chain.Block {
	h := uint32(len(n.Chain))
	script := make([]byte, 32)
	if pay != nil {
		copy(script, pay[:])
	}
	cb := &chain.Tx{Version: 1, Inputs: []chain.TxIn{{PrevIndex: h}},
		Outputs: []chain.TxOut{{Value: 40 * chain.POWM, Script: script}}}
	txs := []*chain.Tx{cb}
	for _, tx := range n.Pool {
		txs = append(txs, tx)
	}
	txs = append(txs, extra...)
	n.Pool = make(map[[32]byte]*chain.Tx)
	hdr := &chain.Header{Version: 1, Height: h, Time: 1_800_000_000 + h*120,
		MerkleRoot: chain.TxMerkleRoot(txs)}
	if h > 0 {
		hdr.PrevBlock = n.Chain[h-1].Header.ID()
	}
	b := &chain.Block{Header: hdr, Txs: txs}
	n.Chain = append(n.Chain, b)
	return b
}

// Fork discards the blocks from height `from` (a reorg) — the next Mine() builds a new branch.
func (n *Node) Fork(from int) { n.Chain = n.Chain[:from] }

func (n *Node) Tip() (uint64, [32]byte, error) {
	b := n.Chain[len(n.Chain)-1]
	return uint64(b.Header.Height), b.Header.ID(), nil
}

func (n *Node) Blocks(from uint64, count int) ([]*chain.Block, error) {
	var out []*chain.Block
	for h := from; h < uint64(len(n.Chain)) && len(out) < count; h++ {
		out = append(out, n.Chain[h])
	}
	return out, nil
}

func (n *Node) Mempool() ([][32]byte, error) {
	var ids [][32]byte
	for id := range n.Pool {
		ids = append(ids, id)
	}
	return ids, nil
}

func (n *Node) Broadcast(tx *chain.Tx) error {
	switch {
	case n.Reject:
		return &refusal{msg: "the input is already spent by another transaction"}
	case n.Busy:
		return &refusal{msg: "the mempool is full", temporary: true}
	case n.Silent:
		return errors.New("timeout awaiting response")
	}
	n.Pool[tx.TxID()] = tx
	if n.LoseReply {
		return errors.New("timeout awaiting response")
	}
	return nil
}
