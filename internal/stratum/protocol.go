// Package stratum is PowMachina's stratum protocol: miner ↔ node (solo with several devices) or
// miner ↔ pool. The specification: the stratum spec.
//
// Line-delimited JSON over TCP (optionally TLS). A client logs in with an address (and a device name),
// the server sends it jobs for every lane separately — the header without a nonce and the share target;
// the client looks for a 64-bit nonce and sends it back. The coinbase is assembled by the server, so a
// miner needs neither the transactions nor any knowledge of the chain.
package stratum

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"powmachina/internal/mh"
	"powmachina/internal/work"
)

// MaxLine is the cap on a message's length — a longer line means the end of the connection.
const MaxLine = 16 << 10

// The submit (and login) error codes.
const (
	ErrCodeStale       = 21
	ErrCodeDuplicate   = 22
	ErrCodeLowDiff     = 23
	ErrCodeNotLoggedIn = 24
	ErrCodeInvalid     = 25
	ErrCodeRateLimit   = 26
	ErrCodeBadLogin    = 27
)

// Request is a message with an id (client→server) or without one (a notification server→client).
type Request struct {
	ID     *uint64         `json:"id,omitempty"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

// Response is the answer to a Request with an id.
type Response struct {
	ID     uint64          `json:"id"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *Error          `json:"error,omitempty"`
}

// Error is an error in an answer.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return fmt.Sprintf("%s (%d)", e.Message, e.Code) }

// LoginParams: the login is the address for the rewards, optionally with a device name after a dot
// ("powm1….rig1"). Lanes are the lanes the client has devices for ("C", "G").
type LoginParams struct {
	Login string   `json:"login"`
	Pass  string   `json:"pass,omitempty"`
	Agent string   `json:"agent,omitempty"`
	Lanes []string `json:"lanes"`
}

// LoginResult is the answer to a login. The jobs arrive right after it as `job` notifications.
type LoginResult struct {
	ID      string `json:"id"`
	Network string `json:"network"`
}

// JobParams is the `job` notification.
type JobParams struct {
	JobID  string `json:"job_id"`
	Lane   string `json:"lane"`
	Height uint64 `json:"height"`
	Epoch  uint64 `json:"epoch"`
	Header string `json:"header"` // the 80 B of header without the nonce, hex
	Target string `json:"target"` // the share target, 32 B big-endian hex (hash ≤ target)
}

// PauseParams is the `pause` notification: there is no work for the lane right now (lane rotation).
type PauseParams struct {
	Lane   string `json:"lane"`
	Reason string `json:"reason"`
}

// SubmitParams: the nonce is a 64-bit number written as 16 hex digits (the number in big-endian form).
type SubmitParams struct {
	JobID string `json:"job_id"`
	Nonce string `json:"nonce"`
}

// SubmitResult is the answer to an accepted submit.
type SubmitResult struct {
	Status string `json:"status"` // "OK"
	Block  bool   `json:"block"`  // this share was also a block
}

// FormatNonce writes a nonce into a submit.
func FormatNonce(n uint64) string { return fmt.Sprintf("%016x", n) }

// ParseNonce reads a nonce from a submit.
func ParseNonce(s string) (uint64, error) {
	if len(s) != 16 {
		return 0, errors.New("nonce must be 16 hex digits")
	}
	return strconv.ParseUint(s, 16, 64)
}

// ParseLane reads a lane.
func ParseLane(s string) (mh.Lane, bool) {
	switch s {
	case "C":
		return mh.LaneC, true
	case "G":
		return mh.LaneG, true
	}
	return 0, false
}

// JobFrom turns a notification into a job for the miner.
func JobFrom(p *JobParams) (*work.Job, error) {
	lane, ok := ParseLane(p.Lane)
	if !ok {
		return nil, fmt.Errorf("stratum: unknown lane %q", p.Lane)
	}
	j := &work.Job{ID: p.JobID, Lane: lane, Height: p.Height, Epoch: p.Epoch}
	h, err := hex.DecodeString(p.Header)
	if err != nil || len(h) != mh.HeaderBytes {
		return nil, errors.New("stratum: header must be 80 bytes hex")
	}
	copy(j.PoW[:], h)
	t, err := hex.DecodeString(p.Target)
	if err != nil || len(t) != 32 {
		return nil, errors.New("stratum: target must be 32 bytes hex")
	}
	copy(j.Target[:], t)
	return j, nil
}

// JobParamsFrom turns a job into a notification (the server).
func JobParamsFrom(j *work.Job) *JobParams {
	return &JobParams{JobID: j.ID, Lane: j.Lane.String(), Height: j.Height, Epoch: j.Epoch,
		Header: hex.EncodeToString(j.PoW[:]), Target: hex.EncodeToString(j.Target[:])}
}
