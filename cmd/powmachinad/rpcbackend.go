package main

import (
	"errors"
	"fmt"
	"math/big"
	"time"

	"powmachina/internal/address"
	"powmachina/internal/chain"
	"powmachina/internal/mh"
	"powmachina/internal/p2p"
	"powmachina/internal/rpc"
	"powmachina/internal/store"
)

// Node implements rpc.Backend — RPC has no view of the state of its own, it only exposes the node's.

func (n *Node) Params() chain.NetParams { return n.params }

// NodeName is the label the operator chose at startup (-nodename). Empty if they chose none.
func (n *Node) NodeName() string { return n.nodeName }

func (n *Node) Tip() (*chain.Header, uint64, [32]byte) { return n.chain.Tip() }

func (n *Node) ChainWork() *big.Int { return n.chain.Work() }

func (n *Node) UTXOCount() int { return n.coinCount() }

func (n *Node) MempoolStats() (int, int) { return n.mempool.Len(), n.mempool.Bytes() }

func (n *Node) MempoolIDs() [][32]byte { return n.mempool.IDs() }

func (n *Node) Block(id [32]byte) (*chain.Block, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	b, err := n.store.Block(id)
	ok := err == nil
	return b, ok
}

func (n *Node) HeaderAt(height uint64) ([32]byte, bool) { return n.chain.BlockIDAt(height) }

// Header implements rpc.Backend: a verified header, even when its body has not arrived yet.
func (n *Node) Header(id [32]byte) (*chain.Header, bool) { return n.chain.HeaderByID(id) }

// TrustedWork says whether the chain meets the network's minimum chain work.
func (n *Node) TrustedWork() bool { return n.chain.TrustedWork() }

// Finality reports the highest finalized height, or "not finalized" when the network has no quorum (the finality spec).
func (n *Node) Finality() (uint64, [32]byte, bool) {
	if n.finality == nil {
		return 0, [32]byte{}, false
	}
	return n.finality.Finalized()
}

// FinalityStatus implements rpc.Backend: the quorum of the block the network is signing now.
func (n *Node) FinalityStatus() rpc.FinalityStatus {
	if n.finality == nil {
		return rpc.FinalityStatus{}
	}
	st := rpc.FinalityStatus{Enabled: true, Window: n.params.FinalityWindow, Key: n.FinalityKey(),
		Caught: len(n.finality.Caught())}
	_, tip, _ := n.chain.Tip()
	if tip < chain.FinalityDepth {
		return st
	}
	st.PendingHeight = tip - chain.FinalityDepth
	id, ok := n.chain.BlockIDAt(st.PendingHeight)
	q := n.quorumAt(st.PendingHeight)
	if !ok || q == nil {
		return st
	}
	st.PendingBlock = id
	st.Members, st.Total = q.Members(), q.Total()
	st.Signed = n.finality.Signed(st.PendingHeight, id, q)
	if st.Key != nil {
		st.Own = q.Weight(*st.Key)
	}
	return st
}

// SubmitTx puts a transaction into the mempool and relays it to the network. The validation
// (including the signatures) is done by the mempool — RPC is not more trustworthy than a peer.
func (n *Node) SubmitTx(tx *chain.Tx) error {
	_, height, _ := n.chain.Tip()
	if _, err := n.mempool.Accept(tx, n.store, height+1); err != nil {
		return err
	}
	n.announce([]p2p.InvItem{{Type: p2p.InvTx, ID: tx.TxID()}}, "")
	return nil
}

func (n *Node) AllowedLanes() []mh.Lane { return n.chain.AllowedLanesNow(time.Now()) }

// SyncState is the sync state for the wallet: peers, an estimate of the target and the
// finished UTXO set.
//
// `bestKnown` is the highest height any peer reported in its handshake. It is a number from
// strangers, so it cannot be relied on when deciding about the chain — but without it progress
// cannot be expressed: the node knows its own tip, not the end.
func (n *Node) SyncState() (int, uint64, uint64) {
	peers, best := n.peers.PeerHeights()
	var applied uint64
	if tip, err := n.store.Tip(); err == nil {
		applied = tip.Height
	}
	return peers, best, applied
}

// HashrateSeries implements rpc.Backend.
func (n *Node) HashrateSeries(blocks, window, points int) chain.HashrateSeries {
	return n.chain.HashrateSeries(blocks, window, points)
}

// Generate mines blocks to an address. Regtest only — on mainnet and testnet it is the miner
// that mines and not the node, and RPC able to produce blocks has no business being there.
func (n *Node) Generate(count int, addr string) ([][32]byte, error) {
	if n.params.Magic != chain.RegTest.Magic {
		return nil, errors.New("generatetoaddress is regtest only")
	}
	n.genMu.Lock()
	defer n.genMu.Unlock()
	n.mu.Lock()
	prev := n.minerKey
	n.mu.Unlock()
	defer func() { n.mu.Lock(); n.minerKey = prev; n.mu.Unlock() }()
	if err := n.SetMinerAddress(addr); err != nil {
		return nil, err
	}
	_, start, _ := n.chain.Tip()
	if err := n.MineBlocks(count); err != nil {
		return nil, err
	}
	var ids [][32]byte
	for h := start + 1; h <= start+uint64(count); h++ {
		if id, ok := n.chain.BlockIDAt(h); ok {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// Template assembles a template for a miner or a pool.
//
// The whole thing runs under applyMu.RLock: the tip, the next difficulty and the selected
// transactions have to come from **one** state. Without it a template can catch the moment
// between "the block is applied" and "the mempool is cleaned", take a transaction the new tip
// has already confirmed, and the block the miner finds from it is rejected by the node —
// see the comment on Node.applyMu.
func (n *Node) Template(lane mh.Lane) (*rpc.Template, error) { return n.template(lane, false) }

// template is Template; sharesOnly lets a lane the rotation rule blocks have a template all the
// same, marked NoBlock, when the node takes shares: a share does not depend on the rotation (it
// is work on its parent, and the Sharepool pays it), so the devices on the node's stratum port go
// on mining shares instead of standing still until the other lane's block.
func (n *Node) template(lane mh.Lane, sharesOnly bool) (*rpc.Template, error) {
	n.applyMu.RLock()
	defer n.applyMu.RUnlock()

	allowed := n.chain.AllowedLanesNow(time.Now())
	ok := false
	for _, l := range allowed {
		if l == lane {
			ok = true
			break
		}
	}
	noBlock := false
	if !ok {
		if !sharesOnly || !n.epochs.isFull() {
			return nil, fmt.Errorf("lane %s is currently blocked by the rotation rule", lane)
		}
		noBlock = true
	}

	// No work is handed out on a chain that has not reached the checkpoints or the work the
	// network is known to have: it is either still being downloaded or it is not the network's.
	if !n.chain.Anchored() {
		return nil, errors.New("the chain is below the network's checkpoints — no work until it has synced")
	}

	tip, height, tipID := n.chain.Tip()
	bits, err := n.chain.NextBitsFor(lane)
	if err != nil {
		return nil, err
	}
	next := height + 1

	// The coinbase keeps its own cap; the rest of the block belongs to transactions.
	txs := n.mempool.Select(chain.MaxBlockBytes-chain.MaxCoinbaseBytes,
		chain.MaxBlockStorageMass-chain.CoinbaseStorageMassReserve)
	ids := make([]string, 0, len(txs))
	raws := make([]string, 0, len(txs))
	var fees int64
	for _, tx := range txs {
		if e, found := n.mempool.Get(tx.TxID()); found {
			fees += e.Fee
		}
		id := tx.TxID()
		ids = append(ids, fmt.Sprintf("%x", id))
		raws = append(raws, fmt.Sprintf("%x", tx.Serialize()))
	}

	minTime := n.chain.MinTime()
	if !n.params.Active(chain.ForkLaneDifficulty, next) && tip.Time >= minTime {
		minTime = tip.Time + 1
	}
	t := &rpc.Template{
		Height:    next,
		Lane:      lane.String(),
		Bits:      fmt.Sprintf("%08x", bits),
		PrevBlock: fmt.Sprintf("%x", tipID),
		MinTime:   minTime,
		Version:   n.params.BlockVersion(next),
		Subsidy:   chain.Subsidy(next),
		DevFee:    chain.DevFee(next),
		Fees:      fees,
		TxIDs:     ids,
		Txs:       raws,
		SizeLimit: chain.MaxBlockBytes - chain.MaxCoinbaseBytes,
	}
	if key := n.FinalityKey(); key != nil {
		t.FinalityKey = fmt.Sprintf("%x", key[:])
	}
	settlement, err := n.templateSettlement(tipID, height)
	if err != nil {
		return nil, err
	}
	if t.Shares = len(settlement.Shares); t.Shares > 0 {
		t.Settlement = fmt.Sprintf("%x", settlement.Serialize())
	}
	// The coinbase pays what the credit state of the tip owes; that state has to be the tip's.
	if st, err := n.store.Tip(); err != nil || st.ID != tipID {
		return nil, errors.New("the state is not at the tip yet — no work until the tip is applied")
	}
	payouts, err := chain.CreditPayouts(next, n.store)
	if err != nil {
		return nil, err
	}
	for _, o := range payouts {
		t.Payouts = append(t.Payouts, rpc.Payout{Value: o.Value, Script: fmt.Sprintf("%x", o.Script)})
	}
	root := n.store.CreditsRoot()
	t.CreditRoot = fmt.Sprintf("%x", root[:])
	t.TakesShares = n.epochs.isFull()
	t.NoBlock = noBlock
	return t, nil
}

// Tx finds a transaction by txid. It needs the index on (-txindex); without it, it returns
// store.ErrNoTxIndex so the caller can tell a missing index from a missing transaction.
func (n *Node) Tx(txid [32]byte) (*chain.Tx, *chain.Block, uint32, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	loc, err := n.store.TxLocation(txid)
	if err != nil {
		return nil, nil, 0, err
	}
	blk, err := n.store.Block(loc.BlockID)
	if err != nil {
		return nil, nil, 0, err
	}
	if int(loc.Index) >= len(blk.Txs) {
		return nil, nil, 0, fmt.Errorf("the txindex points at transaction %d, the block has %d",
			loc.Index, len(blk.Txs))
	}
	return blk.Txs[loc.Index], blk, loc.Index, nil
}

// AddressOf translates an output script into a network address.
func (n *Node) AddressOf(scriptVersion uint8, script []byte) (string, error) {
	return address.Encode(n.params.Bech32HRP, scriptVersion, script)
}

// Address returns a summary of an address and its history. The address is first decoded into a
// script, which gives the index key — the index knows nothing about addresses, only scripts.
func (n *Node) Address(addr string, before uint64, limit int) (store.AddrSummary, []store.AddrEntry, error) {
	version, program, err := address.Decode(n.params.Bech32HRP, addr)
	if err != nil {
		return store.AddrSummary{}, nil, fmt.Errorf("invalid address: %w", err)
	}
	sk := store.ScriptKeyFor(version, program)

	n.mu.Lock()
	defer n.mu.Unlock()
	sum, err := n.store.AddrSummary(sk)
	if err != nil {
		return store.AddrSummary{}, nil, err
	}
	entries, err := n.store.AddrHistory(sk, before, limit)
	if err != nil {
		return store.AddrSummary{}, nil, err
	}
	return sum, entries, nil
}

// RichList returns the richest addresses and the total supply.
//
// The supply comes from the sum of the UTXO set, not from the emission curve: the curve says
// how much should have come into existence, the UTXO set says how much really exists. For a
// share of the rich list the latter is the right one.
func (n *Node) RichList(limit int) ([]store.RichEntry, int64, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	entries, err := n.store.RichList(limit)
	if err != nil {
		return nil, 0, err
	}
	var supply int64
	if err := n.store.ForEachCoin(func(_ chain.Outpoint, c chain.Coin) error {
		supply += c.Value
		return nil
	}); err != nil {
		return nil, 0, err
	}
	return entries, supply, nil
}

// LaneStats is the state of the individual lanes.
func (n *Node) LaneStats() chain.LaneStats { return n.chain.LaneStats() }

// ForkStatus implements rpc.Backend.
func (n *Node) ForkStatus() chain.ForkStatus { return n.chain.ForkStatus() }

// Coin returns an unspent output if it still exists.
func (n *Node) Coin(op chain.Outpoint) (chain.Coin, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.store.Coin(op)
}
