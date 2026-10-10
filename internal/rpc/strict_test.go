package rpc

import (
	"strings"
	"testing"
)

// Parameters at the top level are an error, not "I will use the default". A silently tolerated
// typo in an API costs whoever is writing a pool or an explorer hours.
func TestRequestRejectsParamsAtTopLevel(t *testing.T) {
	if _, err := decodeRequest([]byte(`{"method":"getblocks","count":60}`)); err == nil {
		t.Fatal("parameters outside \"params\" should have been refused")
	} else if !strings.Contains(err.Error(), "params") {
		t.Fatalf("the error should say where the parameters belong: %v", err)
	}
	// The correct shape has to pass.
	if _, err := decodeRequest([]byte(`{"method":"getblocks","params":{"count":60},"id":1}`)); err != nil {
		t.Fatalf("a correct request failed: %v", err)
	}
	// And so does one without parameters.
	if _, err := decodeRequest([]byte(`{"method":"getinfo"}`)); err != nil {
		t.Fatalf("a request without parameters failed: %v", err)
	}
}

// The same one level down: an unknown method parameter has to be reported.
func TestParamsRejectUnknownField(t *testing.T) {
	var p struct {
		Count int `json:"count"`
	}
	if err := decodeParams([]byte(`{"limit":60}`), &p); err == nil {
		t.Fatal("an unknown parameter should have been refused")
	}
	if err := decodeParams([]byte(`{"count":60}`), &p); err != nil || p.Count != 60 {
		t.Fatalf("a correct parameter failed: %v (count %d)", err, p.Count)
	}
}

// Strictness must not shoot down our own clients: `internal/nodeclient` (and with it the
// explorer, the wallet and the miner) sends a JSON-RPC 2.0 envelope including the "jsonrpc" field.
//
// A node that refused every request carrying "jsonrpc" would turn the explorer into a wall of
// 404s.
func TestRequestAcceptsRealClientEnvelope(t *testing.T) {
	bodies := []string{
		`{"jsonrpc":"2.0","id":1,"method":"getinfo","params":null}`,
		`{"jsonrpc":"2.0","id":7,"method":"getblocks","params":{"count":5}}`,
		`{"method":"getinfo"}`,
	}
	for _, b := range bodies {
		req, err := decodeRequest([]byte(b))
		if err != nil {
			t.Fatalf("envelope %s should have passed: %v", b, err)
		}
		if req.Method == "" {
			t.Fatalf("the method was not read from %s", b)
		}
	}
}
