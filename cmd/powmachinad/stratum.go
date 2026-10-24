package main

import (
	"errors"
	"fmt"

	"powmachina/internal/chain"
	"powmachina/internal/mh"
	"powmachina/internal/stratum"
	"powmachina/internal/work"
)

// The node as the backend of the stratum server (-stratum): miners connect to the node and
// mine solo, every device with its own coinbase. the stratum spec.

// WorkTemplate implements stratum.Backend.
func (n *Node) WorkTemplate(lane mh.Lane) (*work.Template, error) {
	t, err := n.template(lane, true)
	if err != nil {
		return nil, err
	}
	payouts := make([]work.Payout, len(t.Payouts))
	for i, p := range t.Payouts {
		payouts[i] = work.Payout{Value: p.Value, Script: p.Script}
	}
	return &work.Template{Height: t.Height, Lane: t.Lane, Bits: t.Bits, PrevBlock: t.PrevBlock,
		MinTime: t.MinTime, Version: t.Version, Fees: t.Fees, Txs: t.Txs, FinalityKey: t.FinalityKey,
		Settlement: t.Settlement, Payouts: payouts, CreditRoot: t.CreditRoot, TakesShares: t.TakesShares,
		NoBlock: t.NoBlock}, nil
}

// TipID implements stratum.Backend.
func (n *Node) TipID() [32]byte {
	_, _, id := n.chain.Tip()
	return id
}

// PoWHash implements stratum.Backend.
func (n *Node) PoWHash(h *chain.Header) ([32]byte, error) {
	s, err := n.epochs.forHeight(uint64(h.Height))
	if err != nil {
		return [32]byte{}, err
	}
	return s.Hash(h)
}

// SetStratum attaches a running stratum server (for getstratuminfo).
func (n *Node) SetStratum(s *stratum.Server) {
	n.mu.Lock()
	n.stratum = s
	n.mu.Unlock()
	n.stratumKick.Store(s)
}

// StratumInfo implements rpc.Backend.
func (n *Node) StratumInfo() (any, error) {
	n.mu.Lock()
	s := n.stratum
	n.mu.Unlock()
	if s == nil {
		return nil, errors.New("stratum is off (start the node with -stratum)")
	}
	workers := s.Stats()
	var hc, hg float64
	for _, w := range workers {
		hc += w.HashrateC
		hg += w.HashrateG
	}
	return map[string]any{"workers": workers, "count": len(workers),
		"hashrate_c": fmt.Sprintf("%.0f", hc), "hashrate_g": fmt.Sprintf("%.0f", hg)}, nil
}

var _ stratum.Backend = (*Node)(nil)
