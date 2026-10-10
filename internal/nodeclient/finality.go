package nodeclient

import (
	"context"
	"fmt"
)

// Finality: what the standalone quorum signer (`powmachina-finality-signer`) needs.

// BlockHashAt returns the ID of the block at the given height of the active branch.
func (c *Client) BlockHashAt(ctx context.Context, height uint64) ([32]byte, error) {
	var out string
	if err := c.Call(ctx, "getblockhash", map[string]any{"height": height}, &out); err != nil {
		return [32]byte{}, err
	}
	return hex32(out)
}

// Finality returns up to which height the chain is confirmed according to the node.
func (c *Client) Finality(ctx context.Context) (uint64, bool, error) {
	var out struct {
		Height    uint64 `json:"height"`
		Finalized bool   `json:"finalized"`
	}
	if err := c.Call(ctx, "getfinality", nil, &out); err != nil {
		return 0, false, err
	}
	return out.Height, out.Finalized, nil
}

// SubmitAttestation sends a finality attestation (hex) to the node. It returns a short summary of the
// response for the signer's log — the node also answers with where finality moved to.
func (c *Client) SubmitAttestation(ctx context.Context, attHex string) (string, error) {
	var out struct {
		Accepted        bool   `json:"accepted"`
		Finalized       bool   `json:"finalized"`
		FinalizedHeight uint64 `json:"finalized_height"`
		FinalizedBlock  string `json:"finalized_block"`
	}
	if err := c.Call(ctx, "submitattestation", map[string]any{"hex": attHex}, &out); err != nil {
		return "", err
	}
	if !out.Accepted {
		return "", fmt.Errorf("%w: the node did not accept the attestation", ErrRPC)
	}
	if out.Finalized {
		return fmt.Sprintf(" — network finality is at height %d", out.FinalizedHeight), nil
	}
	return " — the quorum is not met yet", nil
}
