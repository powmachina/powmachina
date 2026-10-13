package main

import (
	"bytes"
	"fmt"
	"net"
	"testing"
	"time"

	"powmachina/internal/chain"
	"powmachina/internal/mh"
	"powmachina/internal/p2p"
)

// Relay: the node has to accept a transaction from a peer, verify it and file it into the
// mempool. Without that a mempool on a single node is of no use to anybody.
func TestNodeAcceptsRelayedTransaction(t *testing.T) {
	params := chain.RegTest
	node := NewNode(params, chain.RegtestGenesis(), mh.TestProfile)
	defer node.Close()

	// The output we are going to spend.
	sk := new([32]byte)
	for i := range sk {
		sk[i] = byte(i + 3)
	}
	pk, err := mh.SchnorrPubkey(sk)
	if err != nil {
		t.Fatal(err)
	}
	op := chain.Outpoint{TxID: [32]byte{55}, Index: 0}
	coin := chain.Coin{Value: 1_000_000, Script: pk[:], Height: 0}
	if err := node.store.AddCoin(op, coin); err != nil {
		t.Fatal(err)
	}

	ln, err := p2p.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go node.Listen(ln)

	conn, err := p2p.Dial(ln.Addr().String(), params.Magic)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ver := &p2p.VersionMsg{Protocol: p2p.ProtocolVersion, Timestamp: time.Now().Unix(),
		Nonce: 0xFEED, UserAgent: "/test/"}
	if _, err := conn.Handshake(ver, 0xFEED, true); err != nil {
		t.Fatal(err)
	}

	// A signed transaction.
	tx := &chain.Tx{Version: 1,
		Inputs:  []chain.TxIn{{PrevTxID: op.TxID, PrevIndex: op.Index}},
		Outputs: []chain.TxOut{{Value: 900_000, Script: pk[:]}}}
	sig, err := chain.SignInput(tx, 0, []chain.Coin{coin}, sk, chain.RegTest.At(1))
	if err != nil {
		t.Fatal(err)
	}
	tx.Witness = []chain.Witness{{Items: [][]byte{sig}}}

	// 1) we offer it over inv and the node should ask for it
	invPayload, err := p2p.EncodeInv(p2p.TxItems([][32]byte{tx.TxID()}))
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Send(&p2p.Message{Command: p2p.CmdInv, Payload: invPayload}); err != nil {
		t.Fatal(err)
	}
	msg, err := conn.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if msg.Command != p2p.CmdGetData {
		t.Fatalf("expected getdata after inv, got %q", msg.Command)
	}
	items, err := p2p.DecodeGetData(msg.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Type != p2p.InvTx || items[0].ID != tx.TxID() {
		t.Fatalf("the node asked for something else: %+v", items)
	}

	// 2) we send the transaction body
	txPayload, err := p2p.EncodeTx(tx)
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Send(&p2p.Message{Command: p2p.CmdTx, Payload: txPayload}); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for node.mempool.Len() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if node.mempool.Len() != 1 {
		t.Fatalf("the transaction did not get into the mempool (%d in the pool)", node.mempool.Len())
	}

	// 3) the node has it now — it must not ask a second time
	if err := conn.Send(&p2p.Message{Command: p2p.CmdInv, Payload: invPayload}); err != nil {
		t.Fatal(err)
	}
	// If it asked again, a getdata would arrive; otherwise nothing comes and Recv hits the timeout.
	done := make(chan *p2p.Message, 1)
	go func() {
		m, err := conn.Recv()
		if err == nil {
			done <- m
		} else {
			done <- nil
		}
	}()
	select {
	case m := <-done:
		if m != nil && m.Command == p2p.CmdGetData {
			t.Fatal("the node is asking for a transaction it already has")
		}
	case <-time.After(500 * time.Millisecond):
		// fine — the node is silent
	}
}

// An invalid transaction must not get into the mempool and must not knock the connection over.
func TestNodeRejectsInvalidRelayedTransaction(t *testing.T) {
	params := chain.RegTest
	node := NewNode(params, chain.RegtestGenesis(), mh.TestProfile)
	defer node.Close()

	ln, err := p2p.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go node.Listen(ln)

	conn, err := p2p.Dial(ln.Addr().String(), params.Magic)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ver := &p2p.VersionMsg{Protocol: p2p.ProtocolVersion, Timestamp: time.Now().Unix(),
		Nonce: 0xBEEF, UserAgent: "/test/"}
	if _, err := conn.Handshake(ver, 0xBEEF, true); err != nil {
		t.Fatal(err)
	}

	// The transaction spends an output that does not exist.
	tx := &chain.Tx{Version: 1,
		Inputs:  []chain.TxIn{{PrevTxID: [32]byte{200}, PrevIndex: 0}},
		Outputs: []chain.TxOut{{Value: 900_000, Script: make([]byte, 32)}},
		Witness: []chain.Witness{{Items: [][]byte{make([]byte, 64)}}}}
	payload, err := p2p.EncodeTx(tx)
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Send(&p2p.Message{Command: p2p.CmdTx, Payload: payload}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if node.mempool.Len() != 0 {
		t.Fatal("an invalid transaction got into the mempool")
	}
	// The connection has to live on — an invalid transaction is not a reason to ban.
	// A ping is exactly an 8 B nonce; the node returns the same payload in the pong.
	nonce := []byte{1, 2, 3, 4, 5, 6, 7, 8}
	if err := conn.Send(&p2p.Message{Command: p2p.CmdPing, Payload: nonce}); err != nil {
		t.Fatalf("the connection died after an invalid transaction: %v", err)
	}
	// A `getdata` may arrive before the pong: the transaction spends an output we do not know,
	// so it ended up in the orphan pool and the node is asking for its parent. That is correct —
	// it just is not the pong we are waiting for here.
	for {
		msg, err := conn.Recv()
		if err != nil {
			t.Fatalf("the node stopped answering: %v", err)
		}
		if msg.Command == p2p.CmdGetData {
			continue
		}
		if msg.Command != p2p.CmdPong || !bytes.Equal(msg.Payload, nonce) {
			t.Fatalf("expected a pong with the same nonce, got %q %x", msg.Command, msg.Payload)
		}
		break
	}
	// The orphan pool is a cap, not a warehouse: the transaction waits in it, it did not get into the mempool.
	if node.orphans.Len() != 1 {
		t.Fatalf("the orphan pool should hold 1 transaction, it holds %d", node.orphans.Len())
	}
}

// A new block has to reach a peer with its body too. This is the path every block takes on a
// running network: it is announced with `inv`, the receiver pulls the headers and only then the
// bodies. When the receiver asks for a body without a header, `acceptBlock` drops it and the
// chain moves nowhere — the miner's node grows and everybody else stays at zero.
func TestMinedBlockReachesPeerWithBody(t *testing.T) {
	miner, minerAddr, cleanup := listeningNode(t)
	defer cleanup()

	follower := NewNode(chain.RegTest, chain.RegtestGenesis(), mh.TestProfile)
	defer follower.Close()
	follower.addrs.Add(minerAddr, net.ParseIP("88.198.113.1"))
	if err := follower.FillOutboundOnce(p2p.PeerOutboundFull); err != nil {
		t.Fatalf("the connection to the mining node failed: %v", err)
	}

	const want = 3
	if err := miner.MineBlocks(want); err != nil {
		t.Fatalf("mining failed: %v", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		_, height, id := follower.chain.Tip()
		if height == want {
			follower.mu.Lock()
			haveBody := follower.store.HasBody(id)
			follower.mu.Unlock()
			if haveBody {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the peer only caught up to height %d of %d", height, want)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// The connection has to survive the relay — a body without a header must neither disconnect nor punish the peer.
	if got := follower.peers.Count(p2p.PeerOutboundFull); got != 1 {
		t.Fatalf("the connection fell apart during the relay (%d)", got)
	}
	if got := miner.peers.Count(p2p.PeerInbound); got != 1 {
		t.Fatalf("the mining node lost its peer (%d)", got)
	}
}

// A body without a verified header is dropped, but it is not a reason for misbehaviour points
// or a disconnect: between requesting the body and its arrival the header can be lost in a reorg.
func TestBodyWithoutHeaderIsIgnoredNotPunished(t *testing.T) {
	node, nodeAddr, cleanup := listeningNode(t)
	defer cleanup()

	// A block from another branch whose header the node has never seen.
	foreign := NewNode(chain.RegTest, chain.RegtestGenesis(), mh.TestProfile)
	defer foreign.Close()
	if err := foreign.MineBlocks(1); err != nil {
		t.Fatal(err)
	}
	_, _, id := foreign.chain.Tip()
	blk, err := foreign.store.Block(id)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := p2p.EncodeBlock(blk)
	if err != nil {
		t.Fatal(err)
	}

	conn, err := p2p.Dial(fmt.Sprintf("%s:%d", nodeAddr.IP, nodeAddr.Port), chain.RegTest.Magic)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ver := &p2p.VersionMsg{Protocol: p2p.ProtocolVersion, Timestamp: time.Now().Unix(),
		Nonce: 0xC0DE, UserAgent: "/test/"}
	if _, err := conn.Handshake(ver, 0xC0DE, true); err != nil {
		t.Fatal(err)
	}
	if err := conn.Send(&p2p.Message{Command: p2p.CmdBlock, Payload: payload}); err != nil {
		t.Fatal(err)
	}

	// The body must not be accepted…
	time.Sleep(300 * time.Millisecond)
	if _, height, _ := node.chain.Tip(); height != 0 {
		t.Fatalf("a body without a header was accepted, height %d", height)
	}
	// …and the connection has to live on.
	nonce := []byte{8, 7, 6, 5, 4, 3, 2, 1}
	if err := conn.Send(&p2p.Message{Command: p2p.CmdPing, Payload: nonce}); err != nil {
		t.Fatalf("the connection died: %v", err)
	}
	pong, err := conn.Recv()
	if err != nil {
		t.Fatalf("the node stopped answering: %v", err)
	}
	if pong.Command != p2p.CmdPong {
		t.Fatalf("expected a pong, got %q", pong.Command)
	}
}
