package nodeclient_test

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"powmachina/internal/chain"
	"powmachina/internal/nodeclient"
	"powmachina/internal/wallet"
)

// fakeNode answers every call with what `answer` returns for the method: a result, an error message, or
// (hangUp) nothing at all — the connection is cut after the request has been read.
func fakeNode(t *testing.T, answer func(method string) (result any, errMsg string, hangUp bool)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var rq struct {
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&rq)
		result, errMsg, hangUp := answer(rq.Method)
		if hangUp {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				conn.Close()
			}
			return
		}
		out := map[string]any{"id": 1}
		if errMsg != "" {
			out["error"] = map[string]any{"code": -32000, "message": errMsg}
		} else {
			out["result"] = result
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(srv.Close)
	return srv
}

var someTx = &chain.Tx{Version: 1, Inputs: []chain.TxIn{{PrevIndex: 1}},
	Outputs: []chain.TxOut{{Value: chain.POWM, Script: make([]byte, 32)}}}

// What the wallet does with a payment hangs on telling "the node said no" from "the node said nothing".
func TestBroadcastTellsARefusalFromALostReply(t *testing.T) {
	refusing := fakeNode(t, func(string) (any, string, bool) {
		return nil, chain.ErrMempoolConflict.Error(), false
	})
	err := nodeclient.New(refusing.URL, "").Broadcast(someTx)
	if err == nil || !wallet.Rejected(err) {
		t.Fatalf("the node's own error has to count as a refusal: %v", err)
	}

	silent := fakeNode(t, func(string) (any, string, bool) { return nil, "", true })
	err = nodeclient.New(silent.URL, "").Broadcast(someTx)
	if err == nil || wallet.Rejected(err) {
		t.Fatalf("a connection cut before the answer says nothing about the transaction: %v", err)
	}

	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "bad gateway", http.StatusBadGateway)
	}))
	defer proxy.Close()
	err = nodeclient.New(proxy.URL, "").Broadcast(someTx)
	if err == nil || wallet.Rejected(err) {
		t.Fatalf("an error page from something in front of the node says nothing about the transaction: %v", err)
	}

	// Nobody listens: the request never left, so the transaction is certainly not out.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	err = nodeclient.New(addr, "").Broadcast(someTx)
	if err == nil || !wallet.Rejected(err) {
		t.Fatalf("a connection that was never made has to count as a refusal: %v", err)
	}
}

// A wallet of one network pointed at a node of another must not read a single block from it: the keys
// are the same everywhere, so it would find "its" coins there and report them as deposits.
func TestAWalletRefusesANodeOnAnotherNetwork(t *testing.T) {
	wallet.DefaultKDF = wallet.KDFParams{MemoryKiB: 8 << 10, Time: 1, Threads: 1}
	network := "testnet"
	blocksAsked := 0
	srv := fakeNode(t, func(method string) (any, string, bool) {
		switch method {
		case "getinfo":
			return map[string]any{"network": network, "height": 0, "tip": strings.Repeat("00", 32)}, "", false
		case "getrawblocks":
			blocksAsked++
		}
		return []any{}, "", false
	})
	w, _, err := wallet.Create(filepath.Join(t.TempDir(), "w"), "password", chain.RegTest, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	node := nodeclient.New(srv.URL, "")
	node.ExpectNetwork(chain.RegTest.Name)

	if _, err := w.Sync(node); !errors.Is(err, nodeclient.ErrWrongNetwork) {
		t.Fatalf("a sync against a testnet node: %v", err)
	}
	if blocksAsked != 0 {
		t.Fatalf("the wallet asked a node on another network for blocks %d times", blocksAsked)
	}
	// The same node on the right network is fine — and so is the same client once the node behind the
	// address has been put right.
	network = chain.RegTest.Name
	if _, err := w.Sync(node); err != nil {
		t.Fatalf("a sync against a node on the wallet's own network: %v", err)
	}
}

// A node without room has refused nothing about the transaction itself; the wallet keeps such a payment.
func TestBroadcastMarksAFullNodeAsTemporary(t *testing.T) {
	for reason, want := range map[error]bool{chain.ErrMempoolFull: true, chain.ErrChainTooLong: true,
		chain.ErrMempoolConflict: false, chain.ErrFeeTooLow: false} {
		srv := fakeNode(t, func(string) (any, string, bool) { return nil, reason.Error() + " (detail)", false })
		err := nodeclient.New(srv.URL, "").Broadcast(someTx)
		var tmp interface{ Temporary() bool }
		if !errors.As(err, &tmp) || tmp.Temporary() != want || !wallet.Rejected(err) {
			t.Fatalf("%q: temporary has to be %v: %v", reason, want, err)
		}
	}
}

// "We already have it" is the answer to a second attempt after a first one that went through.
func TestBroadcastTakesAKnownTransactionAsSent(t *testing.T) {
	srv := fakeNode(t, func(string) (any, string, bool) {
		return nil, chain.ErrMempoolDuplicate.Error(), false
	})
	if err := nodeclient.New(srv.URL, "").Broadcast(someTx); err != nil {
		t.Fatalf("a transaction the node already holds was reported as failed: %v", err)
	}
}
