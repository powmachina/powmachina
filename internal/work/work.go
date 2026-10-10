// Package work is the job for a miner: the header without a nonce, the target and the lane — and, when
// the coinbase is assembled by whoever produced the job (a miner solo against a node, the stratum server
// in a node), the block's transactions too. One piece of code for both sides, so a block from a miner
// and a block from stratum are assembled the same way.
package work

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"powmachina/internal/chain"
	"powmachina/internal/mh"
)

// Template is a block template from gettemplate.
type Template struct {
	Height    uint64   `json:"height"`
	Lane      string   `json:"lane"`
	Bits      string   `json:"bits"`
	PrevBlock string   `json:"prev_block"`
	MinTime   uint32   `json:"min_time"`
	Fees      int64    `json:"fees"`
	Txs       []string `json:"txs"`
}

// Job is work for a miner.
type Job struct {
	ID     string // the job's identifier (stratum); empty when mining solo
	Lane   mh.Lane
	Height uint64
	Epoch  uint64
	Target [32]byte             // what the miner is looking for: the block's target, or an easier share target
	PoW    [mh.HeaderBytes]byte // the header without a nonce — the PoW input

	// Only when the job was produced by whoever assembles the block:
	Header      chain.Header
	Txs         []*chain.Tx
	BlockTarget [32]byte
}

// ErrChainAhead: the chain's time is ahead of the clock by more than the node allows — it would refuse the block.
var ErrChainAhead = errors.New("chain time is ahead of the clock — waiting")

// Build assembles a job from a template: the coinbase to `payout` with a random extranonce (two jobs with
// the same address never hash the same thing), the transactions from the template, the merkle root, the header.
func Build(t *Template, payout []byte, params chain.NetParams) (*Job, error) {
	// A block with a time past the bound is refused by the node ("the time is too far in the future"). It
	// only happens when blocks come faster than the clock (regtest, a sharp rise in hashrate before a
	// retarget); 10 s of margin for a clock difference.
	if limit := time.Now().Add(chain.MaxFutureDrift - 10*time.Second).Unix(); int64(t.MinTime) > limit {
		return nil, ErrChainAhead
	}
	bits, err := ParseBits(t.Bits)
	if err != nil {
		return nil, err
	}
	target, err := chain.CompactToTarget(bits)
	if err != nil {
		return nil, err
	}
	prev, err := hex.DecodeString(t.PrevBlock)
	if err != nil || len(prev) != 32 {
		return nil, errors.New("work: template has an invalid prev_block")
	}
	var extra [32]byte
	if _, err := rand.Read(extra[:]); err != nil {
		return nil, err
	}
	txs := []*chain.Tx{chain.BuildCoinbase(t.Height, t.Fees, payout, extra, params)}
	for i, h := range t.Txs {
		raw, err := hex.DecodeString(h)
		if err != nil {
			return nil, fmt.Errorf("work: template transaction %d is not hex", i)
		}
		tx, used, err := chain.DeserializeTx(raw)
		if err != nil || used != len(raw) {
			return nil, fmt.Errorf("work: template transaction %d cannot be decoded", i)
		}
		txs = append(txs, tx)
	}
	lane := mh.LaneC
	if t.Lane == "G" {
		lane = mh.LaneG
	}
	j := &Job{Lane: lane, Height: t.Height, Epoch: chain.EpochForHeight(t.Height), Target: target,
		BlockTarget: target, Txs: txs}
	j.Header = chain.Header{Version: 1, Bits: bits, Height: uint32(t.Height), Lane: lane,
		MerkleRoot: chain.TxMerkleRoot(txs), Time: max(uint32(time.Now().Unix()), t.MinTime)}
	copy(j.Header.PrevBlock[:], prev)
	j.PoW = j.Header.PoWInput()
	return j, nil
}

// Block assembles the block with the nonce found (only for a job with its own coinbase).
func (j *Job) Block(nonce uint64) *chain.Block {
	h := j.Header
	h.Nonce = nonce
	return &chain.Block{Header: &h, Txs: j.Txs}
}

// ParseBits reads a compact target in hex ("1f00ffff").
func ParseBits(s string) (uint32, error) {
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 4 {
		return 0, fmt.Errorf("work: invalid bits %q", s)
	}
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3]), nil
}
