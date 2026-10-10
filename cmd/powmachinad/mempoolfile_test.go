package main

import (
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"powmachina/internal/chain"
	"powmachina/internal/mh"
	"powmachina/internal/p2p"
	"powmachina/internal/store"
)

// fundedKey puts a spendable output into storage and returns the key and the outpoint.
func fundedKey(t *testing.T, n *Node, seed byte, value int64) (*[32]byte, []byte, chain.Outpoint, chain.Coin) {
	t.Helper()
	sk := new([32]byte)
	for i := range sk {
		sk[i] = seed + byte(i)
	}
	pk, err := mh.SchnorrPubkey(sk)
	if err != nil {
		t.Fatal(err)
	}
	op := chain.Outpoint{TxID: [32]byte{seed, 0xAA}, Index: 0}
	coin := chain.Coin{Value: value, Script: pk[:], Height: 0}
	if err := n.store.AddCoin(op, coin); err != nil {
		t.Fatal(err)
	}
	return sk, pk[:], op, coin
}

func spend(t *testing.T, sk *[32]byte, pk []byte, op chain.Outpoint, coin chain.Coin,
	value int64) *chain.Tx {
	t.Helper()
	tx := &chain.Tx{Version: 1,
		Inputs:  []chain.TxIn{{PrevTxID: op.TxID, PrevIndex: op.Index}},
		Outputs: []chain.TxOut{{Value: value, Script: pk}}}
	sig, err := chain.SignInput(tx, 0, []chain.Coin{coin}, sk)
	if err != nil {
		t.Fatal(err)
	}
	tx.Witness = []chain.Witness{{Items: [][]byte{sig}}}
	return tx
}

// The mempool has to survive a restart. Without that every restart throws away transactions
// that are not in a block yet — and whoever sent them does not know: their wallet already says
// "sent".
func TestMempoolSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	node, _, err := OpenNode(chain.RegTest, chain.RegtestGenesis(), mh.TestProfile, dir)
	if err != nil {
		t.Fatal(err)
	}
	sk, pk, op, coin := fundedKey(t, node, 11, 1_000_000)
	tx := spend(t, sk, pk, op, coin, 900_000)
	if err := node.SubmitTx(tx); err != nil {
		t.Fatal(err)
	}
	if node.mempool.Len() != 1 {
		t.Fatalf("the pool should hold 1 transaction, it holds %d", node.mempool.Len())
	}
	node.Close()

	// The file has to be in the datadir and must not be readable by anybody else.
	path := filepath.Join(dir, mempoolFile)
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("the mempool dump was not found: %v", err)
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Errorf("the dump has permissions %o, expected 600", perm)
	}

	again, _, err := OpenNode(chain.RegTest, chain.RegtestGenesis(), mh.TestProfile, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if again.mempool.Len() != 1 {
		t.Fatalf("after the restart the pool holds %d transactions, expected 1", again.mempool.Len())
	}
	if _, ok := again.mempool.Get(tx.TxID()); !ok {
		t.Error("the transaction was not found in the pool after the restart")
	}
	// The file is deleted after loading: otherwise the same one would be loaded at every
	// startup and transactions would come back to life in the pool even after being confirmed.
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("the mempool dump was not deleted after loading")
	}
}

// A transaction that made it into a block in the meantime is not loaded from the dump:
// `Accept` refuses it, because its input is no longer in the UTXO set.
func TestMempoolRestartDropsConfirmed(t *testing.T) {
	dir := t.TempDir()
	node, _, err := OpenNode(chain.RegTest, chain.RegtestGenesis(), mh.TestProfile, dir)
	if err != nil {
		t.Fatal(err)
	}
	sk, pk, op, coin := fundedKey(t, node, 23, 1_000_000)
	tx := spend(t, sk, pk, op, coin, 900_000)
	if err := node.SubmitTx(tx); err != nil {
		t.Fatal(err)
	}
	node.Close()

	// The output the transaction spends disappears from the UTXO set — exactly what a block does to it.
	st, err := store.Open(filepath.Join(dir, "chain"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.RemoveCoin(op); err != nil {
		t.Fatal(err)
	}
	st.Close()

	again, _, err := OpenNode(chain.RegTest, chain.RegtestGenesis(), mh.TestProfile, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if again.mempool.Len() != 0 {
		t.Fatalf("a confirmed transaction came back into the pool (%d in the pool)", again.mempool.Len())
	}
}

// A child that arrived before its parent must not disappear. The child and the parent travel
// the network independently, through different peers and paths of different lengths, so with
// every chained payment the order is a matter of chance.
func TestOrphanChildAcceptedAfterParentArrives(t *testing.T) {
	node, addr, cleanup := listeningNode(t)
	defer cleanup()

	sk, pk, op, coin := fundedKey(t, node, 37, 1_000_000)
	parent := spend(t, sk, pk, op, coin, 900_000)
	pid := parent.TxID()
	child := spend(t, sk, pk, chain.Outpoint{TxID: pid, Index: 0},
		chain.Coin{Value: 900_000, Script: pk, Height: 0}, 800_000)

	conn, err := p2p.Dial(net.JoinHostPort(addr.IP.String(), strconv.Itoa(int(addr.Port))),
		chain.RegTest.Magic)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ver := &p2p.VersionMsg{Protocol: p2p.ProtocolVersion, Timestamp: time.Now().Unix(),
		Nonce: 0xC01D, UserAgent: "/test/"}
	if _, err := conn.Handshake(ver, 0xC01D, true); err != nil {
		t.Fatal(err)
	}

	send := func(tx *chain.Tx) {
		payload, err := p2p.EncodeTx(tx)
		if err != nil {
			t.Fatal(err)
		}
		if err := conn.Send(&p2p.Message{Command: p2p.CmdTx, Payload: payload}); err != nil {
			t.Fatal(err)
		}
	}

	// The child first — the node should set it aside and ask for the parent.
	send(child)
	waitFor(t, 3*time.Second, func() bool { return node.orphans.Len() == 1 },
		"the child did not end up in the orphan pool")
	if node.mempool.Len() != 0 {
		t.Fatalf("the orphan got into the mempool (%d)", node.mempool.Len())
	}
	if !node.orphans.Has(child.TxID()) {
		t.Error("the orphan pool does not know about the child")
	}

	// Then the parent — and the child has to follow it on its own.
	send(parent)
	waitFor(t, 5*time.Second, func() bool { return node.mempool.Len() == 2 },
		"the child did not get into the mempool after the parent arrived")
	if node.orphans.Len() != 0 {
		t.Errorf("%d transactions remained in the orphan pool", node.orphans.Len())
	}
	if _, ok := node.mempool.Get(child.TxID()); !ok {
		t.Error("the child is not in the mempool")
	}
}

func waitFor(t *testing.T, limit time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal(msg)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
