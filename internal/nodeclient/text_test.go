package nodeclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// answering returns a client of a server that answers every call with `answer`.
func answering(t *testing.T, answer func(w http.ResponseWriter)) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { answer(w) }))
	t.Cleanup(srv.Close)
	return New(srv.URL, "")
}

// The errors of this client are printed: the wallet shows them, the miner and the signer log them.
// The text in them is the node's — or that of whatever answers at the node's address — so it has
// to arrive as text of a sensible length: no terminal escapes, no line breaks, not megabytes.
func TestErrorTextFromTheNodeIsPlainAndBounded(t *testing.T) {
	esc := string(rune(0x1b))
	hostile := "no such block" + esc + "[2J" + esc + "[1A" + "\r\n" + "BLOCK FOUND and accepted" + strings.Repeat("x", 1<<20)

	answers := map[string]func(w http.ResponseWriter){
		"an RPC error": func(w http.ResponseWriter) {
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": hostile}})
		},
		"an HTTP error": func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(hostile))
		},
		"an ID that is not one": func(w http.ResponseWriter) {
			_ = json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"height": 1, "tip": hostile}})
		},
	}
	for name, answer := range answers {
		_, _, err := answering(t, answer).Tip()
		if !errors.Is(err, ErrRPC) {
			t.Fatalf("%s: %v", name, err)
		}
		text := err.Error()
		if !strings.Contains(text, "no such block") {
			t.Errorf("%s: the error lost the node's reason: %q", name, text)
		}
		if len(text) > 2*maxNodeText {
			t.Errorf("%s: the error is %d bytes long", name, len(text))
		}
		for _, r := range text {
			if r < 0x20 || r == 0x7f {
				t.Errorf("%s: the error holds the control character %U", name, r)
				break
			}
		}
	}
}

// An ordinary error from the node reads as the node wrote it.
func TestOrdinaryErrorFromTheNodeIsUnchanged(t *testing.T) {
	c := answering(t, func(w http.ResponseWriter) {
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "the block does not extend the tip"}})
	})
	err := c.Call(context.Background(), "submitblock", nil, nil)
	if err == nil || err.Error() != "node: the block does not extend the tip" {
		t.Fatalf("%v", err)
	}
}
