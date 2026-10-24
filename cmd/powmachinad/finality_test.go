package main

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"powmachina/internal/chain"
	"powmachina/internal/mh"
	"powmachina/internal/p2p"
	"powmachina/internal/rpc"
)

// rpcCall calls a method on the node's RPC server the way a client does — including the
// JSON-RPC 2.0 envelope that `internal/nodeclient` sends.
func rpcCall(t *testing.T, srv http.Handler, method string, params any) (json.RawMessage, error) {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": method, "params": params,
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	var out struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("the answer cannot be read (%d): %s", rec.Code, rec.Body.String())
	}
	if out.Error != nil {
		return nil, fmt.Errorf("%s", out.Error.Message)
	}
	return out.Result, nil
}

// finalityBlocks is the shortest regtest chain with a block that can be final: a full window
// below it and the depth above it.
const finalityBlocks = int(chain.RegtestFinalityWindow + chain.FinalityDepth + 1)

// quietNode is a node that puts its key into its blocks but does not sign: the test signs for it.
func quietNode(t *testing.T) *Node {
	t.Helper()
	n := NewNode(chain.RegTest, chain.RegtestGenesis(), mh.TestProfile)
	n.noAttest = true
	t.Cleanup(n.Close)
	return n
}

// pendingOf is the block at the finality depth below the node's tip.
func pendingOf(t *testing.T, n *Node) (uint64, [32]byte) {
	t.Helper()
	_, height, _ := n.chain.Tip()
	target := height - chain.FinalityDepth
	id, ok := n.chain.BlockIDAt(target)
	if !ok {
		t.Fatalf("no block at height %d", target)
	}
	return target, id
}

// A mining node is the whole quorum of its own chain: its key is in every coinbase, so it signs
// the block at the depth and the block is final — with nothing else running.
func TestAMiningNodeFinalizesItsOwnChain(t *testing.T) {
	node := NewNode(chain.RegTest, chain.RegtestGenesis(), mh.TestProfile)
	defer node.Close()
	if err := node.MineBlocks(finalityBlocks + 3); err != nil {
		t.Fatal(err)
	}
	target, id := pendingOf(t, node)
	if got, gotID, ok := node.finality.Finalized(); !ok || got != target || gotID != id {
		t.Fatalf("finality %d (%v), expected %d", got, ok, target)
	}
	// The key is in the share commitment of every block it mined.
	blk, err := node.store.Block(id)
	if err != nil {
		t.Fatal(err)
	}
	if key, ok := blk.FinalityKey(); !ok || key != *node.FinalityKey() {
		t.Fatal("the node's key is not in its block")
	}
	// A reorg below a finalized block has to be refused.
	if err := node.finality.CheckReorg(target - 1); err != chain.ErrFinalityRewrite {
		t.Fatalf("a reorg below a finalized block: %v", err)
	}
}

// The quorum must not finalize a block the node does not have — not even its own key.
func TestFinalityIgnoresForeignBlock(t *testing.T) {
	node := quietNode(t)
	if err := node.MineBlocks(finalityBlocks); err != nil {
		t.Fatal(err)
	}
	target, id := pendingOf(t, node)
	_ = node.acceptAttestation(chain.Sign(node.signer.Priv(), target, [32]byte{0xff}), "")
	if _, _, ok := node.finality.Finalized(); ok {
		t.Fatal("a block the node does not have was finalized")
	}
	// Its own block, signed by the same key... is a second block at that height: the key is caught.
	_ = node.acceptAttestation(chain.Sign(node.signer.Priv(), target, id), "")
	if _, _, ok := node.finality.Finalized(); ok {
		t.Fatal("a key caught signing twice finalized")
	}
	if len(node.finality.Caught()) != 1 {
		t.Fatal("the double signature was not caught")
	}
	// The chain keeps running.
	if err := node.MineBlocks(1); err != nil {
		t.Fatalf("the chain stopped after a double signature: %v", err)
	}
}

// A key without a seat is not heard: nothing is stored and nothing finalized.
func TestAKeyWithoutASeatIsNotHeard(t *testing.T) {
	node := quietNode(t)
	if err := node.MineBlocks(finalityBlocks); err != nil {
		t.Fatal(err)
	}
	target, id := pendingOf(t, node)
	_, stranger, _ := ed25519.GenerateKey(nil)
	_ = node.acceptAttestation(chain.Sign(stranger, target, id), "")
	if _, _, ok := node.finality.Finalized(); ok {
		t.Fatal("a key without a seat finalized")
	}
	if got := node.finality.InRange(0, target+100, 100); len(got) != 0 {
		t.Fatalf("%d attestations of a stranger stored", len(got))
	}
}

// What is final stays final across a restart, and so does a caught key.
func TestFinalitySurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	node, _, err := OpenNode(chain.RegTest, chain.RegtestGenesis(), mh.TestProfile, dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := node.MineBlocks(finalityBlocks + 2); err != nil {
		t.Fatal(err)
	}
	final, finalID, ok := node.finality.Finalized()
	if !ok {
		t.Fatal("nothing final before the restart")
	}
	_, other, _ := ed25519.GenerateKey(nil)
	ev := &chain.Equivocation{Height: 5, First: chain.Sign(other, 5, [32]byte{1}), Second: chain.Sign(other, 5, [32]byte{2})}
	ev.Key = ev.First.Key
	node.acceptEquivocation(ev, "")
	key := *node.FinalityKey()
	node.Close()

	node, _, err = OpenNode(chain.RegTest, chain.RegtestGenesis(), mh.TestProfile, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer node.Close()
	if got, gotID, ok := node.finality.Finalized(); !ok || got != final || gotID != finalID {
		t.Fatalf("after the restart finality is %d (%v), was %d", got, ok, final)
	}
	if len(node.finality.Caught()) != 1 {
		t.Fatal("the caught key was forgotten")
	}
	if *node.FinalityKey() != key {
		t.Fatal("the node's key changed")
	}
}

// What an exchange and the operator see: the quorum, the node's own share, and the key in the template.
func TestGetFinalityShowsTheQuorum(t *testing.T) {
	node := NewNode(chain.RegTest, chain.RegtestGenesis(), mh.TestProfile)
	defer node.Close()
	if err := node.MineBlocks(finalityBlocks + 1); err != nil {
		t.Fatal(err)
	}
	srv := rpc.NewServer(node, "")
	raw, err := rpcCall(t, srv, "getfinality", nil)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Finalized bool   `json:"finalized"`
		Height    uint64 `json:"height"`
		Window    uint64 `json:"window"`
		Key       string `json:"key"`
		Quorum    struct {
			Height  uint64 `json:"height"`
			Members int    `json:"members"`
			Signed  int    `json:"signed_permille"`
			Own     int    `json:"own_permille"`
		} `json:"quorum"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	target, _ := pendingOf(t, node)
	key := node.FinalityKey()
	if !out.Finalized || out.Height != target || out.Window != chain.RegtestFinalityWindow ||
		out.Key != hex.EncodeToString(key[:]) || out.Quorum.Members != 1 || out.Quorum.Signed != 1000 ||
		out.Quorum.Own != 1000 {
		t.Fatalf("getfinality: %s", raw)
	}

	raw, err = rpcCall(t, srv, "gettemplate", map[string]any{"lane": "C"})
	if err != nil {
		t.Fatal(err)
	}
	var tpl struct {
		FinalityKey string `json:"finality_key"`
	}
	if err := json.Unmarshal(raw, &tpl); err != nil || tpl.FinalityKey != out.Key {
		t.Fatalf("the template carries finality key %q, the node has %q", tpl.FinalityKey, out.Key)
	}
}

// An attestation received from a peer has to be relayed on — otherwise finality would never spread.
func TestAttestationIsRelayed(t *testing.T) {
	params := chain.RegTest
	node := quietNode(t)
	if err := node.MineBlocks(finalityBlocks); err != nil {
		t.Fatal(err)
	}
	target, id := pendingOf(t, node)

	ln, err := p2p.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go node.Listen(ln)

	// We connect two peers; the first sends the attestation, the second has to receive it.
	connA, err := p2p.Dial(ln.Addr().String(), params.Magic)
	if err != nil {
		t.Fatal(err)
	}
	defer connA.Close()
	if _, err := connA.Handshake(&p2p.VersionMsg{Protocol: p2p.ProtocolVersion,
		Timestamp: time.Now().Unix(), Nonce: 11, UserAgent: "/A/"}, 11, true); err != nil {
		t.Fatal(err)
	}
	connB, err := p2p.Dial(ln.Addr().String(), params.Magic)
	if err != nil {
		t.Fatal(err)
	}
	defer connB.Close()
	if _, err := connB.Handshake(&p2p.VersionMsg{Protocol: p2p.ProtocolVersion,
		Timestamp: time.Now().Unix(), Nonce: 22, UserAgent: "/B/"}, 22, true); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool {
		node.mu.Lock()
		defer node.mu.Unlock()
		return len(node.conns) == 2
	}, "the node did not register both connections")

	att := chain.Sign(node.signer.Priv(), target, id)
	payload, err := p2p.EncodeAttestation(att)
	if err != nil {
		t.Fatal(err)
	}
	if err := connA.Send(&p2p.Message{Command: p2p.CmdAttestation, Payload: payload}); err != nil {
		t.Fatal(err)
	}

	got := make(chan *p2p.Message, 1)
	go func() {
		m, err := connB.Recv()
		if err == nil {
			got <- m
		}
	}()
	select {
	case m := <-got:
		if m.Command != p2p.CmdAttestation {
			t.Fatalf("the second peer got %q, expected an attestation", m.Command)
		}
		relayed, err := p2p.DecodeAttestation(m.Payload)
		if err != nil {
			t.Fatal(err)
		}
		if relayed.Height != target || relayed.BlockID != id || relayed.Key != att.Key {
			t.Fatalf("the relayed attestation does not match: %+v", relayed)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the attestation was not relayed to the second peer")
	}
	if got, _, ok := node.finality.Finalized(); !ok || got != target {
		t.Fatalf("finality %d (%v), expected %d", got, ok, target)
	}
}

// A miner on the stratum port with its own key: the node tells it which block to sign when the key
// has a seat, takes its signature, and that signature finalizes — the same as the node's own would.
func TestStratumMinerSignsThroughTheNode(t *testing.T) {
	node := quietNode(t)
	if err := node.MineBlocks(finalityBlocks); err != nil {
		t.Fatal(err)
	}
	target, id := pendingOf(t, node)
	// A key without a seat is asked for nothing.
	if _, _, ok := node.AttestDue([32]byte{9}); ok {
		t.Fatal("a key without a seat was asked to sign")
	}
	// The node's key has every seat here; it stands in for a miner that logged in with it.
	h, due, ok := node.AttestDue(node.signer.Pub)
	if !ok || h != target || due != id {
		t.Fatalf("due: height %d ok %v, expected %d", h, ok, target)
	}
	// A signature of the wrong block is refused.
	if err := node.SubmitAttestation(chain.Sign(node.signer.Priv(), target, [32]byte{0xee})); err == nil {
		t.Fatal("a signature of a block the node does not have was taken")
	}
	fresh := quietNode(t)
	if err := fresh.MineBlocks(finalityBlocks); err != nil {
		t.Fatal(err)
	}
	ft, fid := pendingOf(t, fresh)
	if err := fresh.SubmitAttestation(chain.Sign(fresh.signer.Priv(), ft, fid)); err != nil {
		t.Fatalf("the miner's signature: %v", err)
	}
	if got, _, ok := fresh.finality.Finalized(); !ok || got != ft {
		t.Fatalf("finality %d (%v), expected %d", got, ok, ft)
	}
	// The same signature again is not the miner's mistake.
	if err := fresh.SubmitAttestation(chain.Sign(fresh.signer.Priv(), ft, fid)); err != nil {
		t.Fatalf("the same signature again: %v", err)
	}
}
