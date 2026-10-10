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
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"powmachina/internal/chain"
	"powmachina/internal/work"
)

// Client talks to one node. Token is the node's RPC token (POWM_RPC_TOKEN).
type Client struct {
	url   string
	token string
	http  *http.Client
	id    atomic.Uint64
}

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
		return fmt.Errorf("node %s is not reachable: %w", c.url, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBytes))
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("%w: invalid RPC token (POWM_RPC_TOKEN)", ErrRPC)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: HTTP %d: %s", ErrRPC, resp.StatusCode, strings.TrimSpace(string(data)))
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
		return fmt.Errorf("%w: %s", ErrRPC, r.Error.Message)
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
		Height uint64 `json:"height"`
		Tip    string `json:"tip"`
	}
	if err := c.Call(ctx, "getinfo", nil, &info); err != nil {
		return 0, [32]byte{}, err
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

// Broadcast implements wallet.Node.
func (c *Client) Broadcast(tx *chain.Tx) error {
	ctx, cancel := c.ctx()
	defer cancel()
	return c.Call(ctx, "sendrawtransaction", map[string]any{"hex": hex.EncodeToString(tx.Serialize())}, nil)
}

func hex32(s string) ([32]byte, error) {
	var out [32]byte
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 32 {
		return out, fmt.Errorf("%w: invalid ID %q", ErrRPC, s)
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
