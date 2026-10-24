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
	"math"
	"time"

	"powmachina/internal/chain"
	"powmachina/internal/mh"
)

// Template is a block template from gettemplate.
type Template struct {
	Height    uint64 `json:"height"`
	Lane      string `json:"lane"`
	Bits      string `json:"bits"`
	PrevBlock string `json:"prev_block"`
	MinTime   uint32 `json:"min_time"`
	// Version is the header version the node wants on the block (the fork signal bits). A node
	// that does not send it gets the base version.
	Version uint32   `json:"version"`
	Fees    int64    `json:"fees"`
	Txs     []string `json:"txs"`
	// FinalityKey is the node's finality key (hex), carried in the share commitment so that the
	// block gives the node a seat in the finality quorum (the finality spec). Empty = no key.
	FinalityKey string `json:"finality_key,omitempty"`
	// Settlement is the settlement body the block carries (hex, chain.Settlement): the shares it
	// pays. Empty = none.
	Settlement string `json:"settlement,omitempty"`
	// Payouts are the credits the coinbase pays after the dev fee, in this order — the front of
	// the payout queue (the Sharepool spec §5.2). A coinbase that pays anything else is invalid.
	Payouts []Payout `json:"payouts,omitempty"`
	// CreditRoot is the root of the credit state the block builds on (hex), which the coinbase
	// commits to.
	CreditRoot string `json:"credit_root"`
	// TakesShares: the node takes shares through submitshare (BuildShares).
	TakesShares bool `json:"takes_shares"`
	// NoBlock: the lane may not have the next block (the rotation rule) — the job is for
	// Sharepool shares only, and a hash that meets the block's target is a share too.
	NoBlock bool `json:"no_block,omitempty"`
}

// Payout is one output of the coinbase's payouts.
type Payout struct {
	Value  int64  `json:"value"`
	Script string `json:"script"` // the payout program, hex
}

// coinbasePayouts reads a template's payouts and credit root.
func coinbasePayouts(t *Template) ([]chain.TxOut, [32]byte, error) {
	var root [32]byte
	raw, err := hex.DecodeString(t.CreditRoot)
	if err != nil || len(raw) != 32 {
		return nil, root, errors.New("work: template has an invalid credit_root")
	}
	copy(root[:], raw)
	outs := make([]chain.TxOut, len(t.Payouts))
	for i, p := range t.Payouts {
		script, err := hex.DecodeString(p.Script)
		if err != nil || len(script) != 32 || p.Value <= 0 {
			return nil, root, fmt.Errorf("work: template payout %d is invalid", i)
		}
		outs[i] = chain.TxOut{Value: p.Value, ScriptVersion: chain.ScriptVersionBasic, Script: script}
	}
	return outs, root, nil
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
	Share       chain.ShareCommit
	Settlement  chain.Settlement
	Txs         []*chain.Tx
	BlockTarget [32]byte
	// BodyRoot is what the header's merkle root commits to besides the share commitment.
	BodyRoot [32]byte
	// ShareTarget is the Sharepool target of the job's share weight (BuildShares). Target starts
	// as it; a stratum server sets its own on top.
	ShareTarget [32]byte
	// Shares: every hash that meets Target is a share of weight Share.Weight for submitshare,
	// and one that also meets BlockTarget is the block (BuildShares). Otherwise Target is the
	// block's.
	Shares bool
	// NoBlock: the lane may not have the next block (Template.NoBlock); every hit is a share.
	NoBlock bool
}

// ErrChainAhead: the chain's time is ahead of the clock by more than the node allows — it would refuse the block.
var ErrChainAhead = errors.New("chain time is ahead of the clock — waiting")

// Build assembles a job from a template: the coinbase to `payout` with a random extranonce (two jobs with
// the same address never hash the same thing), the transactions from the template, the share
// commitment, the merkle root, the header. The job looks for a block, the share of the heaviest
// weight.
func Build(t *Template, payout []byte, params chain.NetParams) (*Job, error) {
	if t.NoBlock {
		return nil, fmt.Errorf("work: lane %s may not have the next block (a template for shares only)", t.Lane)
	}
	return build(t, payout, chain.MaxShareWeight, params)
}

// BuildShares is Build for a miner that publishes its shares (the Sharepool spec §2.2): the share
// commitment carries `weight`, and the job's Target is the share target of that weight. A node that
// takes no shares (Template.TakesShares) gets a job that looks for the block alone.
func BuildShares(t *Template, payout []byte, weight uint8, params chain.NetParams) (*Job, error) {
	if !t.TakesShares {
		if t.NoBlock {
			return nil, fmt.Errorf("work: lane %s may not have the next block and the node takes no shares", t.Lane)
		}
		return Build(t, payout, params)
	}
	if weight > chain.MaxShareWeight {
		return nil, fmt.Errorf("work: share weight %d is above %d", weight, chain.MaxShareWeight)
	}
	j, err := build(t, payout, weight, params)
	if err != nil {
		return nil, err
	}
	if j.ShareTarget, err = chain.ShareTarget(j.Header.Bits, weight); err != nil {
		return nil, err
	}
	j.Target, j.Shares, j.NoBlock = j.ShareTarget, true, t.NoBlock
	return j, nil
}

func build(t *Template, payout []byte, weight uint8, params chain.NetParams) (*Job, error) {
	// The payout is under the PoW (the share commitment), and a commitment carries the program of a
	// version 0 address: 32 B.
	if len(payout) != 32 {
		return nil, fmt.Errorf("work: the payout program is %d B, a version 0 address has 32", len(payout))
	}
	// A block with a time past the bound is refused by the node ("the time is too far in the future"). It
	// only happens when blocks come faster than the clock (regtest, a sharp rise in hashrate before a
	// retarget); 10 s of margin for a clock difference.
	if limit := time.Now().Add(params.FutureDrift(t.Height) - 10*time.Second).Unix(); int64(t.MinTime) > limit {
		return nil, ErrChainAhead
	}
	// A header carries its height in 32 bits. A template past that cannot become a block, and its
	// epoch is not one anything can be built for.
	if t.Height > math.MaxUint32 {
		return nil, fmt.Errorf("work: template height %d does not fit a header", t.Height)
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
	payouts, creditRoot, err := coinbasePayouts(t)
	if err != nil {
		return nil, err
	}
	var extra [32]byte
	if _, err := rand.Read(extra[:]); err != nil {
		return nil, err
	}
	txs := []*chain.Tx{chain.BuildCoinbase(t.Height, payouts, extra, params)}
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
	var commit chain.ShareCommit
	copy(commit.Payout[:], payout)
	commit.Weight = weight
	if t.FinalityKey != "" {
		raw, err := hex.DecodeString(t.FinalityKey)
		if err != nil || len(raw) != 32 {
			return nil, errors.New("work: template has an invalid finality_key")
		}
		copy(commit.FinalityKey[:], raw)
	}
	chain.SealCoinbase(txs, creditRoot)
	var settlement chain.Settlement
	if t.Settlement != "" {
		raw, err := hex.DecodeString(t.Settlement)
		if err != nil {
			return nil, errors.New("work: template settlement is not hex")
		}
		s, used, err := chain.DeserializeSettlement(raw)
		if err != nil || used != len(raw) {
			return nil, fmt.Errorf("work: template settlement cannot be decoded: %v", err)
		}
		settlement = *s
	}
	lane := mh.LaneC
	if t.Lane == "G" {
		lane = mh.LaneG
	}
	j := &Job{Lane: lane, Height: t.Height, Epoch: chain.EpochForHeight(t.Height), Target: target,
		BlockTarget: target, Share: commit, Settlement: settlement, Txs: txs}
	version := t.Version
	if version == 0 {
		version = chain.VersionBase
	}
	j.BodyRoot = chain.BodyRoot(txs, &settlement)
	j.Header = chain.Header{Version: version, Bits: bits, Height: uint32(t.Height), Lane: lane,
		MerkleRoot: chain.ShareRoot(j.BodyRoot, &commit), Time: max(uint32(time.Now().Unix()), t.MinTime)}
	copy(j.Header.PrevBlock[:], prev)
	j.PoW = j.Header.PoWInput()
	return j, nil
}

// Block assembles the block with the nonce found (only for a job with its own coinbase).
func (j *Job) Block(nonce uint64) *chain.Block {
	h := j.Header
	h.Nonce = nonce
	return &chain.Block{Header: &h, Share: j.Share, Settlement: j.Settlement, Txs: j.Txs}
}

// ShareAt is the share the nonce found (only for a job with its own coinbase).
func (j *Job) ShareAt(nonce uint64) *chain.Share {
	h := j.Header
	h.Nonce = nonce
	return &chain.Share{Header: h, BodyRoot: j.BodyRoot, Commit: j.Share}
}

// ParseBits reads a compact target in hex ("1f00ffff").
func ParseBits(s string) (uint32, error) {
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 4 {
		return 0, fmt.Errorf("work: invalid bits %q", s)
	}
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3]), nil
}
