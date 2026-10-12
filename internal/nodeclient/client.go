// Package nodeclient is a JSON-RPC client for powmachinad. The wallet uses it
// (wallet-cli, wallet-rpc) — it implements wallet.Node.
package nodeclient

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"powmachina/internal/chain"
	"powmachina/internal/safetext"
	"powmachina/internal/work"
)

// Client talks to one node. Token is the node's RPC token (POWM_RPC_TOKEN).
type Client struct {
	url   string
	token string
	http  *http.Client
	id    atomic.Uint64
	// network: the network the node has to be on (ExpectNetwork); empty = whatever it is on.
	network string
}

// ErrWrongNetwork: the node is on another network than the caller works on.
var ErrWrongNetwork = errors.New("node is on another network")

// ExpectNetwork makes the client refuse a node on any other network (mainnet, testnet, regtest). Call it
// before the client is shared.
//
// A wallet needs this more than anybody: the keys are the same on every network and only the prefix of
// the address differs, so a mainnet wallet pointed at a testnet node by one wrong port would scan the
// testnet chain and report its worthless coins as confirmed deposits. Tip checks it, and every sync
// starts with Tip — a node swapped for another one later is caught the same way as a wrong one at start.
func (c *Client) ExpectNetwork(name string) { c.network = name }

// New creates a client. `addr` is host:port or a full http(s):// URL.
func New(addr, token string) *Client {
	url := addr
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		url = "http://" + url
	}
	return &Client{url: url, token: token, http: &http.Client{Timeout: 60 * time.Second}}
}

// MaxResponseBytes: 100 blocks of 256 kB in hex is ~52 MB; the cap has some headroom.
const MaxResponseBytes = 128 << 20

// ErrRPC is an error the node returned (not a network error).
var ErrRPC = errors.New("node")

// refusal is a failure that leaves no doubt the node did not do what was asked: it answered with an
// error of its own, it turned the token down, or no connection to it was ever made. A timeout, a broken
// connection or an error page from something in between are NOT refusals — the request may have been
// carried out and only the answer lost. The wallet decides by this whether a payment may be forgotten
// (wallet.Rejected).
type refusal struct {
	err error
	// temporary: the node has no room for the transaction right now; nothing is wrong with it.
	temporary bool
}

func (r *refusal) Error() string   { return r.err.Error() }
func (r *refusal) Unwrap() error   { return r.err }
func (r *refusal) Rejected() bool  { return true }
func (r *refusal) Temporary() bool { return r.temporary }

// maxNodeText is how much of a text the node wrote ends up in an error.
const maxNodeText = 500

// fromNode makes a text from the node's answer fit to carry in an error. The errors are printed —
// by the wallet, the miner, the signer — and the answer is whatever is at the other end of the
// address the client was given: a body of megabytes, or one with terminal escapes in it.
func fromNode(s string) string {
	if len(s) > 2*maxNodeText {
		s = s[:2*maxNodeText]
	}
	return safetext.Line(strings.TrimSpace(s), maxNodeText)
}

// Call calls a method and unpacks the result into `out` (which may be nil).
func (c *Client) Call(ctx context.Context, method string, params, out any) error {
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": c.id.Add(1), "method": method, "params": params})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		err = fmt.Errorf("node %s is not reachable: %w", c.url, err)
		// A connection that was never made carried nothing; one that broke later may have.
		var op *net.OpError
		if errors.As(err, &op) && op.Op == "dial" {
			return &refusal{err: err}
		}
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBytes))
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return &refusal{err: fmt.Errorf("%w: invalid RPC token (POWM_RPC_TOKEN)", ErrRPC)}
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: HTTP %d: %s", ErrRPC, resp.StatusCode, fromNode(string(data[:min(len(data), 2*maxNodeText)])))
	}
	var r struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return fmt.Errorf("%w: unreadable response", ErrRPC)
	}
	if r.Error != nil {
		return &refusal{err: fmt.Errorf("%w: %s", ErrRPC, fromNode(r.Error.Message))}
	}
	if out == nil || len(r.Result) == 0 {
		return nil
	}
	return json.Unmarshal(r.Result, out)
}

func (c *Client) ctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 60*time.Second)
}

// Tip implements wallet.Node.
func (c *Client) Tip() (uint64, [32]byte, error) {
	ctx, cancel := c.ctx()
	defer cancel()
	var info struct {
		Network string `json:"network"`
		Height  uint64 `json:"height"`
		Tip     string `json:"tip"`
	}
	if err := c.Call(ctx, "getinfo", nil, &info); err != nil {
		return 0, [32]byte{}, err
	}
	if c.network != "" && info.Network != c.network {
		return 0, [32]byte{}, fmt.Errorf("%w: %s reports %q, expected %s", ErrWrongNetwork, c.url, info.Network, c.network)
	}
	id, err := hex32(info.Tip)
	return info.Height, id, err
}

// Blocks implements wallet.Node. Every block is parsed and its ID verified against what the node
// claims — a body that does not match the header is not used.
func (c *Client) Blocks(from uint64, count int) ([]*chain.Block, error) {
	ctx, cancel := c.ctx()
	defer cancel()
	var raw []struct {
		Height uint64 `json:"height"`
		ID     string `json:"id"`
		Hex    string `json:"hex"`
	}
	if err := c.Call(ctx, "getrawblocks", map[string]any{"from": from, "count": count}, &raw); err != nil {
		return nil, err
	}
	out := make([]*chain.Block, 0, len(raw))
	for _, r := range raw {
		data, err := hex.DecodeString(r.Hex)
		if err != nil {
			return nil, fmt.Errorf("%w: block %d is not hex", ErrRPC, r.Height)
		}
		b, err := chain.DeserializeBlock(data)
		if err != nil {
			return nil, fmt.Errorf("%w: block %d: %w", ErrRPC, r.Height, err)
		}
		id, err := hex32(r.ID)
		if err != nil || b.Header.ID() != id || uint64(b.Header.Height) != r.Height {
			return nil, fmt.Errorf("%w: block %d does not match its ID", ErrRPC, r.Height)
		}
		if chain.TxMerkleRoot(b.Txs) != b.Header.MerkleRoot {
			return nil, fmt.Errorf("%w: block %d has transactions that do not match its header", ErrRPC, r.Height)
		}
		out = append(out, b)
	}
	return out, nil
}

// Mempool implements wallet.Node.
func (c *Client) Mempool() ([][32]byte, error) {
	ctx, cancel := c.ctx()
	defer cancel()
	var ids []string
	if err := c.Call(ctx, "getrawmempool", nil, &ids); err != nil {
		return nil, err
	}
	out := make([][32]byte, 0, len(ids))
	for _, s := range ids {
		id, err := hex32(s)
		if err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, nil
}

// Broadcast implements wallet.Node. A node that already holds the transaction has not refused it: that is
// the answer to the second attempt after a first one whose reply was lost, and it means success.
func (c *Client) Broadcast(tx *chain.Tx) error {
	ctx, cancel := c.ctx()
	defer cancel()
	err := c.Call(ctx, "sendrawtransaction", map[string]any{"hex": hex.EncodeToString(tx.Serialize())}, nil)
	var r *refusal
	if !errors.As(err, &r) {
		return err
	}
	// The node's RPC carries its reasons as text only, so the text of the mempool's own errors is what
	// there is to tell them apart by.
	said := func(reason error) bool { return strings.Contains(err.Error(), reason.Error()) }
	switch {
	case said(chain.ErrMempoolDuplicate):
		return nil
	case said(chain.ErrMempoolFull), said(chain.ErrChainTooLong):
		return &refusal{err: r.err, temporary: true}
	}
	return err
}

func hex32(s string) ([32]byte, error) {
	var out [32]byte
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 32 {
		return out, fmt.Errorf("%w: invalid ID %q", ErrRPC, safetext.Line(s, 80))
	}
	copy(out[:], b)
	return out, nil
}

// Template is a block template from gettemplate.
type Template = work.Template

// Template asks for a template for a lane ("C" or "G").
func (c *Client) Template(lane string) (*Template, error) {
	ctx, cancel := c.ctx()
	defer cancel()
	var t Template
	if err := c.Call(ctx, "gettemplate", map[string]any{"lane": lane}, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

// SubmitBlock submits a mined block.
func (c *Client) SubmitBlock(b *chain.Block) error {
	ctx, cancel := c.ctx()
	defer cancel()
	return c.Call(ctx, "submitblock", map[string]any{"hex": hex.EncodeToString(b.Serialize())}, nil)
}

// Network returns the name of the node's network (mainnet, testnet, regtest).
func (c *Client) Network() (string, error) {
	ctx, cancel := c.ctx()
	defer cancel()
	var info struct {
		Network string `json:"network"`
	}
	err := c.Call(ctx, "getinfo", nil, &info)
	return info.Network, err
}
