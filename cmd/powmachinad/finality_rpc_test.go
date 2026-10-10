package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

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

// A standalone signer sends attestations over RPC. The whole path has to work all the way to
// advancing finality: the quorum has its keys outside the node, and the node verifies the
// signature, not the caller.
func TestSubmitAttestationAdvancesFinality(t *testing.T) {
	node := NewNode(chain.RegTest, chain.RegtestGenesis(), mh.TestProfile)
	defer node.Close()
	if err := node.MineBlocks(int(chain.FinalityDepth) + 1); err != nil {
		t.Fatal(err)
	}
	srv := rpc.NewServer(node, "")

	_, height, _ := node.chain.Tip()
	target := height - chain.FinalityDepth
	id, ok := node.chain.BlockIDAt(target)
	if !ok {
		t.Fatalf("we do not have the block at height %d", target)
	}

	// Signers below the threshold must not advance finality.
	for i := 0; i < chain.FinalityThreshold-1; i++ {
		att := chain.Sign(chain.RegtestSignerKey(i), uint8(i), target, id)
		raw, err := p2p.EncodeAttestation(att)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := rpcCall(t, srv, "submitattestation", map[string]any{"hex": hex.EncodeToString(raw)}); err != nil {
			t.Fatalf("attestation %d: %v", i, err)
		}
	}
	if _, _, ok := node.finality.Finalized(); ok {
		t.Fatal("nothing may be finalized below the threshold")
	}

	// The last signature reaches the threshold and the answer has to say so.
	last := chain.FinalityThreshold - 1
	att := chain.Sign(chain.RegtestSignerKey(last), uint8(last), target, id)
	raw, err := p2p.EncodeAttestation(att)
	if err != nil {
		t.Fatal(err)
	}
	res, err := rpcCall(t, srv, "submitattestation", map[string]any{"hex": hex.EncodeToString(raw)})
	if err != nil {
		t.Fatalf("the last attestation: %v", err)
	}
	var out struct {
		Accepted        bool   `json:"accepted"`
		Finalized       bool   `json:"finalized"`
		FinalizedHeight uint64 `json:"finalized_height"`
		FinalizedBlock  string `json:"finalized_block"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		t.Fatal(err)
	}
	if !out.Accepted || !out.Finalized || out.FinalizedHeight != target {
		t.Fatalf("the answer says %+v, expected finality at height %d", out, target)
	}
	if out.FinalizedBlock != hex.EncodeToString(id[:]) {
		t.Fatalf("the answer reports block %s, expected %x", out.FinalizedBlock, id)
	}
	if got, gotID, ok := node.finality.Finalized(); !ok || got != target || gotID != id {
		t.Fatalf("the node has finality %d (%v), expected %d", got, ok, target)
	}
}

// A corrupted attestation has to be refused by RPC with an explanation, not dropped silently.
func TestSubmitAttestationRejectsGarbage(t *testing.T) {
	node := NewNode(chain.RegTest, chain.RegtestGenesis(), mh.TestProfile)
	defer node.Close()
	srv := rpc.NewServer(node, "")

	for _, tc := range []struct{ name, hexStr string }{
		{"not hex", "not-hex"},
		{"empty", ""},
		{"short", "aabbcc"},
		{"long", hex.EncodeToString(make([]byte, p2p.AttestationBytes+1))},
	} {
		if _, err := rpcCall(t, srv, "submitattestation", map[string]any{"hex": tc.hexStr}); err == nil {
			t.Fatalf("%s: should have been refused", tc.name)
		}
	}
	// The right length, but a signature from nobody in the quorum: the node drops it and does
	// not advance finality (it is not a reason for an error — it may be an old quorum).
	var zero [p2p.AttestationBytes]byte
	if _, err := rpcCall(t, srv, "submitattestation",
		map[string]any{"hex": hex.EncodeToString(zero[:])}); err != nil {
		t.Fatalf("an attestation with a bad signature must not be an RPC error: %v", err)
	}
	if _, _, ok := node.finality.Finalized(); ok {
		t.Fatal("an attestation with a bad signature advanced finality")
	}
}

// When the network has no quorum (testnet before the keys, or --no-finality), RPC has to say so.
func TestSubmitAttestationWithoutQuorum(t *testing.T) {
	params := chain.RegTest
	params.FinalitySigners = nil // exactly what --no-finality does
	node := NewNode(params, chain.RegtestGenesis(), mh.TestProfile)
	defer node.Close()
	srv := rpc.NewServer(node, "")

	att := chain.Sign(chain.RegtestSignerKey(0), 0, 1, [32]byte{1})
	raw, err := p2p.EncodeAttestation(att)
	if err != nil {
		t.Fatal(err)
	}
	_, err = rpcCall(t, srv, "submitattestation", map[string]any{"hex": hex.EncodeToString(raw)})
	if err == nil {
		t.Fatal("without a quorum RPC should have refused the attestation")
	}
}
