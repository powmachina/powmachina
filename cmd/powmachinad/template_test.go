package main

import (
	"encoding/hex"
	"net"
	"slices"
	"sync"
	"testing"
	"time"

	"powmachina/internal/chain"
	"powmachina/internal/mh"
	"powmachina/internal/p2p"
)

// What these tests guard: a found block thrown away by the node with `invalid body:
// transaction 1: the input does not exist or is already spent`. That is not a stale share —
// SubmitBlock checks the parent first and would say so. It is a template built in the window
// between "the block is applied" (the tip moves) and "the mempool is cleaned", which carries
// the new tip together with a transaction that very tip has already confirmed. The error
// message points at a transaction, so the cause is hard to see from the log; what is lost is
// the miner's reward.

// A template must not be handed out while a block is being applied. This is the whole fix:
// the tip and the mempool are one state, and Template reads them under applyMu.RLock.
func TestTemplateWaitsWhileABlockIsApplied(t *testing.T) {
	node := NewNode(chain.RegTest, chain.RegtestGenesis(), mh.TestProfile)
	defer node.Close()
	if err := node.MineBlocks(1); err != nil {
		t.Fatal(err)
	}

	// Stand in for applying a block: holding applyMu is exactly what SubmitBlock and
	// ApplyReadyBodies do around moving the tip and cleaning the mempool.
	node.applyMu.Lock()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for _, lane := range []mh.Lane{mh.LaneG, mh.LaneC} {
			if _, err := node.Template(lane); err == nil {
				return
			}
		}
	}()

	select {
	case <-done:
		node.applyMu.Unlock()
		t.Fatal("the template was built while a block was being applied — it can carry a " +
			"transaction that the new tip has already confirmed")
	case <-time.After(250 * time.Millisecond):
	}

	node.applyMu.Unlock()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the template did not get built even after the application finished")
	}
}

// And the behaviour itself: while blocks carrying transactions are being mined, no template
// may ever pair a tip with a transaction that tip already contains. Without the lock this
// fails under `go test -race`; the window is short, so the test mines several blocks.
func TestTemplateNeverCarriesAConfirmedTransaction(t *testing.T) {
	node := NewNode(chain.RegTest, chain.RegtestGenesis(), mh.TestProfile)
	defer node.Close()

	const rounds = 6
	type snap struct {
		prev  string
		txids []string
	}
	var (
		mu    sync.Mutex
		seen  []snap
		stop  = make(chan struct{})
		ended = make(chan struct{})
	)
	go func() {
		defer close(ended)
		for {
			select {
			case <-stop:
				return
			default:
			}
			for _, lane := range []mh.Lane{mh.LaneG, mh.LaneC} {
				tpl, err := node.Template(lane)
				if err != nil {
					continue
				}
				mu.Lock()
				seen = append(seen, snap{prev: tpl.PrevBlock, txids: tpl.TxIDs})
				mu.Unlock()
			}
		}
	}()

	// Every round: one spendable output, one transaction out of it into the mempool, one block
	// that confirms it. The block id and the transaction id are recorded as a forbidden pair.
	forbidden := map[string]string{} // block id -> txid it confirmed
	for i := 0; i < rounds; i++ {
		sk, pk, op, coin := fundedKey(t, node, byte(40+i*7), 1_000_000)
		tx := spend(t, sk, pk, op, coin, 900_000)
		if err := node.SubmitTx(tx); err != nil {
			t.Fatalf("round %d: the transaction was not accepted: %v", i, err)
		}
		if err := node.MineBlocks(1); err != nil {
			t.Fatalf("round %d: %v", i, err)
		}
		_, _, tipID := node.chain.Tip()
		txid := tx.TxID()
		forbidden[hex.EncodeToString(tipID[:])] = hex.EncodeToString(txid[:])
	}

	close(stop)
	<-ended

	mu.Lock()
	defer mu.Unlock()
	if len(seen) == 0 {
		t.Fatal("not a single template was built — the test did not test anything")
	}
	for _, s := range seen {
		bad, ok := forbidden[s.prev]
		if !ok {
			continue
		}
		for _, id := range s.txids {
			if id == bad {
				t.Fatalf("the template on top of block %s carries transaction %s, which that "+
					"very block confirmed — the miner's block would be rejected as invalid body",
					s.prev[:12], id[:12])
			}
		}
	}
	t.Logf("%d templates checked over %d blocks", len(seen), rounds)
}

// best_known has to grow with the chain. version.StartHeight alone is the height at the moment
// of connecting and is never refreshed: after a few hours a node would report a best_known far
// below its own height, and no progress indicator could be computed from it.
func TestBestKnownHeightGrowsAfterTheHandshake(t *testing.T) {
	src, srcAddr, cleanup := listeningNode(t)
	defer cleanup()
	if err := src.MineBlocks(2); err != nil {
		t.Fatal(err)
	}

	node := NewNode(chain.RegTest, chain.RegtestGenesis(), mh.TestProfile)
	defer node.Close()
	node.addrs.Add(srcAddr, net.ParseIP("88.198.113.1"))
	if err := node.FillOutboundOnce(p2p.PeerOutboundFull); err != nil {
		t.Fatal(err)
	}

	// The handshake happened at height 2. Now the source grows — this is the part StartHeight
	// alone never shows.
	if err := src.MineBlocks(5); err != nil {
		t.Fatal(err)
	}

	stop := make(chan struct{})
	defer close(stop)
	node.startLoop(stop, 20*time.Millisecond, func() {
		for _, c := range node.connSnapshot() {
			_ = node.RequestHeaders(c)
		}
		node.SyncBodiesOnce(time.Now())
	})

	deadline := time.Now().Add(15 * time.Second)
	for {
		_, best, _ := node.SyncState()
		if best >= 7 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("best_known stayed at %d, the peer is at 7", best)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// The header version is chosen by the node (it carries the fork signal bits), so it has to reach
// the miner on every path work leaves the node by: the RPC template, the stratum template and
// the node's own miner.
func TestTemplateCarriesTheBlockVersion(t *testing.T) {
	node := NewNode(chain.RegTest, chain.RegtestGenesis(), mh.TestProfile)
	defer node.Close()
	if err := node.MineBlocks(1); err != nil {
		t.Fatal(err)
	}
	tip, height, _ := node.chain.Tip()
	if want := node.params.BlockVersion(height); tip.Version != want {
		t.Fatalf("the node mined version %#x at height %d, expected %#x", tip.Version, height, want)
	}

	want := node.params.BlockVersion(height + 1)
	var checked int
	for _, lane := range []mh.Lane{mh.LaneG, mh.LaneC} {
		tpl, err := node.Template(lane)
		if err != nil {
			continue // the rotation rule blocks one lane at a time
		}
		if tpl.Version != want {
			t.Fatalf("lane %s: the RPC template has version %#x, expected %#x", lane, tpl.Version, want)
		}
		wt, err := node.WorkTemplate(lane)
		if err != nil {
			t.Fatalf("lane %s: %v", lane, err)
		}
		if wt.Version != want {
			t.Fatalf("lane %s: the stratum template has version %#x, expected %#x", lane, wt.Version, want)
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no lane gave a template — the test did not test anything")
	}
}

// A lane the rotation rule blocks has no template for gettemplate, but the node's stratum port gets
// one for shares only (NoBlock) when the node takes shares.
func TestBlockedLaneHasASharesOnlyTemplate(t *testing.T) {
	node := NewNode(chain.RegTest, chain.RegtestGenesis(), mh.TestProfile)
	defer node.Close()
	var blocked mh.Lane
	found := false
	for i := 0; i < 8 && !found; i++ {
		if err := node.SubmitBlock(mineBlockWith(t, node, nil)); err != nil {
			t.Fatal(err)
		}
		allowed := node.chain.AllowedLanesNow(time.Now())
		for _, l := range []mh.Lane{mh.LaneC, mh.LaneG} {
			if !slices.Contains(allowed, l) {
				blocked, found = l, true
			}
		}
	}
	if !found {
		t.Skip("the rotation relief keeps both lanes open on this clock")
	}
	if _, err := node.Template(blocked); err == nil {
		t.Fatal("gettemplate gave a template for a blocked lane")
	}
	tpl, err := node.template(blocked, true)
	if node.epochs.isFull() {
		if err != nil || !tpl.NoBlock || !tpl.TakesShares {
			t.Fatalf("a full node's stratum: %+v, %v; want a shares-only template", tpl, err)
		}
	} else if err == nil {
		t.Fatal("a light node gave a shares-only template")
	}
}
