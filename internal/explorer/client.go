// Package explorer is the PowMachina blockchain explorer.
//
// It keeps no copy of the chain of its own. Every page is assembled from the node's answers over its
// native JSON-RPC — the explorer is stateless.
//
// The reason is worth writing down: a derived copy in a database diverges from
// the chain, and it diverges silently. Negative balances, an address summary lagging 40 minutes behind
// the tip and a transaction's value instead of the value belonging to the address — all of those were
// bugs of the layer between the node and the screen. That layer is not here.
//
// The price is that the explorer is exactly as fast as the node and has nothing to show when it is
// down. That is a conscious trade: better an empty page than numbers that do not match.
package explorer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// MaxResponseBytes is the cap on the node's answer. The node is ours, but the explorer hangs on the
// public web and must not be brought down by one request that asks for a lot of data.
const MaxResponseBytes = 8 << 20

// Client is the JSON-RPC client to the node.
type Client struct {
	url   string
	token string
	http  *http.Client
}

func NewClient(url, token string, timeout time.Duration) *Client {
	return &Client{
		url:   url,
		token: token,
		http:  &http.Client{Timeout: timeout},
	}
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return e.Message }

// Err is an error returned by the node. The explorer distinguishes it from a connection error: "I do
// not know that transaction" is a 404 answer, "the node does not answer" is a 502.
type Err struct{ Msg string }

func (e *Err) Error() string { return e.Msg }

// Call calls a method and parses the result into out.
func (c *Client) Call(ctx context.Context, method string, params any, out any) error {
	req := map[string]any{"jsonrpc": "2.0", "id": 1, "method": method}
	if params != nil {
		req["params"] = params
	}
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	hreq.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		hreq.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(hreq)
	if err != nil {
		return fmt.Errorf("the node does not answer: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBytes))
	if err != nil {
		return fmt.Errorf("the node's answer cannot be read: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("the node returned %s", resp.Status)
	}

	var envelope struct {
		Result json.RawMessage `json:"result"`
		Error  *rpcError       `json:"error"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fmt.Errorf("the node's answer is not JSON: %w", err)
	}
	if envelope.Error != nil {
		return &Err{Msg: envelope.Error.Message}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(envelope.Result, out)
}
