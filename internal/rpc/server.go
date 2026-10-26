// Package rpc is the node's interface for an exchange, the explorer, a pool and the wallet.
//
// The security assumption is simple: **RPC must never be public.** It listens on localhost, and
// if a token is set, every request has to carry it. The request body has a hard cap and the
// server has timeouts — otherwise one slow connection is enough and the node has one more problem.
package rpc

import (
	"bytes"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"sort"
	"strings"
	"time"

	"powmachina/internal/chain"
	"powmachina/internal/mh"
	"powmachina/internal/store"
)

const (
	// MaxRequestBytes is the cap on the request body (a transaction fits with room to spare).
	MaxRequestBytes = 1 << 20
	// MaxBlockListCount is the cap for getblocks. Without it a single request would force the
	// node to deserialize the whole chain.
	MaxBlockListCount = 100
	ReadTimeout       = 15 * time.Second
	WriteTimeout      = 30 * time.Second
)

// Backend is what RPC needs from the node.
type Backend interface {
	Params() chain.NetParams
	NodeName() string
	// Tx finds a transaction by txid; it also returns the block and the position in it.
	// Without the transaction index on, it returns an error — that is a configuration state,
	// not "the transaction does not exist".
	Tx(txid [32]byte) (*chain.Tx, *chain.Block, uint32, error)
	// AddressOf translates an output script into a network address.
	AddressOf(scriptVersion uint8, script []byte) (string, error)
	// Address returns a summary of an address and its history from the most recent movement.
	// Without the address index on, it returns an error.
	Address(addr string, before uint64, limit int) (store.AddrSummary, []store.AddrEntry, error)
	// RichList returns addresses sorted by balance and the total supply, so a share can be
	// computed. Without the address index it returns an error.
	RichList(limit int) ([]store.RichEntry, int64, error)
	// Coin returns an unspent output if it still exists.
	Coin(op chain.Outpoint) (chain.Coin, bool)
	// LaneStats is the state of the individual lanes — difficulty, the share of blocks and an
	// estimated hashrate. It is the one thing a generic explorer cannot show.
	LaneStats() chain.LaneStats
	// ForkStatus is the network's fork table and what the miners signal in the header version.
	ForkStatus() chain.ForkStatus
	// HashrateSeries is the hashrate over time per lane — the graph in the explorer.
	HashrateSeries(blocks, window, points int) chain.HashrateSeries
	Tip() (*chain.Header, uint64, [32]byte)
	ChainWork() *big.Int
	// TrustedWork says whether the chain has at least as much work as the network expects.
	// Neither an exchange nor a wallet should work on a chain below that — it may be cheap fake history.
	TrustedWork() bool
	// ClockSkew is the machine's clock offset from the peers' time and the number of samples.
	ClockSkew() (time.Duration, int)
	// SyncState is what the wallet needs for a **real** progress indicator: how many peers the
	// node has, how far the chain reaches according to them and the height the UTXO set is
	// finished up to. Without a target "I am at 40 %" cannot be said — the node knows its own
	// tip, not the end.
	SyncState() (peers int, bestKnown, applied uint64)
	UTXOCount() int
	MempoolStats() (count, bytes int)
	MempoolIDs() [][32]byte
	Block(id [32]byte) (*chain.Block, bool)
	// Header is the verified header of a block whose body we may not have yet. Without it a
	// block list would have to skip such a height — and a list that silently closes a gap says
	// two blocks are neighbours when they are not (which makes the lane order look broken).
	Header(id [32]byte) (*chain.Header, bool)
	HeaderAt(height uint64) ([32]byte, bool)
	Finality() (height uint64, id [32]byte, ok bool)
	FinalityStatus() FinalityStatus
	SubmitTx(tx *chain.Tx) error
	Template(lane mh.Lane) (*Template, error)
	AllowedLanes() []mh.Lane
	// StratumInfo is the devices connected to the node's stratum port.
	StratumInfo() (any, error)
	// ChecksEveryShare says whether the node verifies the PoW of every share a block settles (a
	// full DAG) or a sample of them (light, the Sharepool spec §7).
	ChecksEveryShare() bool
	// SubmitBlock accepts a block from a miner.
	SubmitBlock(blk *chain.Block) error
	// SubmitShare takes a Sharepool share from a miner: it is verified, pooled and relayed. It
	// answers whether the share also meets its block's target — the miner then submits the block.
	SubmitShare(s *chain.Share) (block bool, err error)
	// SharePool sums the node's share pool by payout program; `full` is false on a node that
	// holds no full DAG and so takes no shares.
	SharePool() (full bool, tally map[[32]byte]chain.ShareTally)
	// Credits is the credit state of the applied tip (the Sharepool spec §5): the pot, the height
	// it is at, how many keys hold a credit and the
	// entries with the most recent last share, at least `limit` of them (more when the cut falls
	// inside one block). With an address, only that address's entry, and the count is 0 or 1.
	Credits(address string, limit int) (pot int64, height uint64, count int, credits []chain.CreditEntry, err error)
	// ShareGeo is where the shares the node took first came from, by country (ISO 3166-1 alpha-2):
	// the last `minutes` and since the start, and the ones whose country is not known. Off without
	// a GeoIP database.
	ShareGeo() (enabled bool, minutes int, window map[string]int, seen map[string]uint64, unknown int, seenUnknown uint64)
	// Generate mines blocks to an address — regtest only (testing wallets and exchanges).
	Generate(count int, address string) ([][32]byte, error)
}

// Template is a template for a miner or a pool.
type Template struct {
	Height    uint64 `json:"height"`
	Lane      string `json:"lane"`
	Bits      string `json:"bits"`
	PrevBlock string `json:"prev_block"`
	MinTime   uint32 `json:"min_time"`
	// Version is the header version for the block: the base and the fork signal bits.
	Version uint32   `json:"version"`
	Subsidy int64    `json:"subsidy"`
	DevFee  int64    `json:"dev_fee"`
	Fees    int64    `json:"fees"`
	TxIDs   []string `json:"tx_ids"`
	// Txs are the transactions in the order they go into the block after the coinbase (hex). The
	// miner assembles the block from them itself — it builds the coinbase with its own address.
	Txs       []string `json:"txs"`
	SizeLimit int      `json:"size_limit"`
	// FinalityKey is the node's finality key (hex). The miner puts it into the coinbase, and the
	// block becomes the node's seat in the finality quorum (the finality spec). Empty = none.
	FinalityKey string `json:"finality_key,omitempty"`
	// Settlement is the settlement body the block carries (hex, chain.Settlement): the shares from
	// the node's pool it pays. The miner commits to it through the body root; empty = none.
	Settlement string `json:"settlement,omitempty"`
	// Shares is how many shares the settlement pays, for the operator to see.
	Shares int `json:"shares"`
	// Payouts are the credits the coinbase pays after the dev fee, in this order; the coinbase
	// pays nothing else (the Sharepool spec §5.2).
	Payouts []Payout `json:"payouts,omitempty"`
	// CreditRoot is the root of the credit state the block builds on (hex); the coinbase commits
	// to it after the witness root.
	CreditRoot string `json:"credit_root"`
	// TakesShares says whether the node takes the miner's shares (submitshare): a node holding
	// the full DAG does, a light one does not, and the miner then looks for blocks only.
	TakesShares bool `json:"takes_shares"`
	// NoBlock: the lane may not have the next block (the rotation rule), so the template is for
	// Sharepool shares only — a share does not depend on the rotation. Only the node's own stratum
	// port asks for such a template; gettemplate answers that the lane is blocked.
	NoBlock bool `json:"no_block,omitempty"`
}

// Payout is one coinbase payout: the value and the payout program (hex).
type Payout struct {
	Value  int64  `json:"value"`
	Script string `json:"script"`
}

// FinalityStatus is the finality quorum as the node sees it.
type FinalityStatus struct {
	Enabled bool
	Window  uint64
	// Active is how recently a key has to have mined to keep its seat (0 = every key of the window).
	Active uint64
	// Key is the node's own finality key, nil when it has none.
	Key *[32]byte
	// Pending is the block at the finality depth below the tip: what the quorum is signing now.
	PendingHeight uint64
	PendingBlock  [32]byte
	// Members, Total, Signed and Own describe the quorum of the pending block: the keys with a
	// seat, the weight of the window, the weight that has signed and the weight of this node's key.
	// Members == 0 = there is no quorum yet (the window is not full).
	Members int
	Total   uint64
	Signed  uint64
	Own     uint64
	// Caught is the number of keys proven to have signed two blocks at one height.
	Caught int
}

type request struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	ID     json.RawMessage `json:"id"`
	// JSONRPC is a field from the JSON-RPC 2.0 standard. We do not use it, but it **has to be
	// here**: we reject unknown fields, and clients (including ours — nodeclient, the wallet,
	// the miner) send it. Without this line every one of them falls off at once.
	JSONRPC string `json:"jsonrpc"`
}

type response struct {
	Result any             `json:"result,omitempty"`
	Error  *rpcError       `json:"error,omitempty"`
	ID     json.RawMessage `json:"id"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type Server struct {
	backend Backend
	token   string
}

func NewServer(backend Backend, token string) *Server {
	return &Server{backend: backend, token: token}
}

// HTTPServer assembles the server with timeouts. `addr` should be localhost.
func (s *Server) HTTPServer(addr string) *http.Server {
	mux := http.NewServeMux()
	mux.Handle("/", s)
	return &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadTimeout:       ReadTimeout,
		WriteTimeout:      WriteTimeout,
		ReadHeaderTimeout: 5 * time.Second,
	}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if s.token != "" {
		got := r.Header.Get("Authorization")
		want := "Bearer " + s.token
		if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
			http.Error(w, "invalid token", http.StatusUnauthorized)
			return
		}
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, MaxRequestBytes+1))
	if err != nil || len(body) > MaxRequestBytes {
		http.Error(w, "the request is too large", http.StatusRequestEntityTooLarge)
		return
	}
	req, err := decodeRequest(body)
	if err != nil {
		writeJSON(w, response{Error: &rpcError{-32600, err.Error()}})
		return
	}

	result, err := s.dispatch(req)
	resp := response{ID: req.ID}
	if err != nil {
		resp.Error = &rpcError{-32000, err.Error()}
	} else {
		resp.Result = result
	}
	writeJSON(w, resp)
}

// decodeRequest reads the request envelope and **rejects unknown fields**.
//
// Why strictly: parameters belong in `params`. When they are written at the top level
// (`{"method":"getblocks","count":60}`), silent tolerance means the node answers with the
// defaults and the caller spends an hour wondering why the API ignores their limit. It costs
// whoever is writing a pool or an explorer their time; it costs us nothing.
func decodeRequest(body []byte) (request, error) {
	var req request
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		if strings.Contains(err.Error(), "unknown field") {
			return req, fmt.Errorf("%s — parameters belong in \"params\"", err.Error())
		}
		return req, errors.New("unreadable JSON")
	}
	return req, nil
}

// decodeParams reads a method's parameters and also rejects unknown fields — for the same reason
// as decodeRequest. `{"params":{"limit":60}}` on a method that expects `count` should say what is
// wrong rather than quietly return the first twenty.
func decodeParams(raw json.RawMessage, out any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		if strings.Contains(err.Error(), "unknown field") {
			return fmt.Errorf("%s — see the chain spec or the method's parameter list", err.Error())
		}
		return err
	}
	return nil
}

func writeJSON(w http.ResponseWriter, resp response) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) dispatch(req request) (any, error) {
	// A positional array is rewritten into a named object before branching on the method — the
	// handlers therefore always see the same shape.
	params, err := normalizeParams(req.Method, req.Params)
	if err != nil {
		return nil, err
	}
	req.Params = params

	switch req.Method {
	case "getinfo":
		return s.getInfo()
	case "getfinality":
		return s.getFinality()
	case "getblock":
		return s.getBlock(req.Params)
	case "getblockcount":
		return s.getBlockCount()
	case "getbestblockhash":
		return s.getBestBlockHash()
	case "getblockhash":
		return s.getBlockHash(req.Params)
	case "getsharepool":
		return s.getSharePool()
	case "getcredits":
		return s.getCredits(req.Params)
	case "getsharegeo":
		return s.getShareGeo()
	case "getblocks":
		return s.getBlocks(req.Params)
	case "gettxout":
		return s.getTxOut(req.Params)
	case "gethashrate":
		return s.getHashrate(req.Params)
	case "getlanestats":
		return s.getLaneStats()
	case "getforks":
		return s.getForks()
	case "getrichlist":
		return s.getRichList(req.Params)
	case "getaddress":
		return s.getAddress(req.Params)
	case "getrawtransaction":
		return s.getRawTransaction(req.Params)
	case "getrawmempool":
		return s.getRawMempool()
	case "sendrawtransaction":
		return s.sendRawTransaction(req.Params)
	case "gettemplate":
		return s.getTemplate(req.Params)
	case "getrawblocks":
		return s.getRawBlocks(req.Params)
	case "generatetoaddress":
		return s.generate(req.Params)
	case "submitblock":
		return s.submitBlock(req.Params)
	case "submitshare":
		return s.submitShare(req.Params)
	case "getstratuminfo":
		return s.backend.StratumInfo()
	default:
		return nil, fmt.Errorf("unknown method %q", req.Method)
	}
}

func (s *Server) getInfo() (any, error) {
	hdr, height, id := s.backend.Tip()
	skew, samples := s.backend.ClockSkew()
	mpCount, mpBytes := s.backend.MempoolStats()
	peers, bestKnown, applied := s.backend.SyncState()
	lanes := make([]string, 0, 3)
	for _, l := range s.backend.AllowedLanes() {
		lanes = append(lanes, l.String())
	}

	return map[string]any{
		"node_name": s.backend.NodeName(),
		"network":   s.backend.Params().Name,
		"height":    height,
		"tip":       hex.EncodeToString(id[:]),
		"tip_time":  hdr.Time,
		"chainwork": s.backend.ChainWork().String(),
		// trusted_work = the chain has at least as much work as the network expects. When it is
		// false, it may be cheap fake history handed to a new node — neither an exchange nor a
		// wallet should work on such a chain.
		"trusted_work":   s.backend.TrustedWork(),
		"utxo_count":     s.backend.UTXOCount(),
		"mempool_txs":    mpCount,
		"mempool_bytes":  mpBytes,
		"allowed_lanes":  lanes,
		"epoch":          mh.EpochForHeight(height),
		"randomx_hw_aes": mh.HasHardwareAES(),
		// time_offset_s: how far this machine's clock is from the network. Beyond ±30 s
		// (MaxFutureDriftStrict) the network stops understanding it.
		"time_offset_s":  int64(skew / time.Second),
		"time_peers":     samples,
		"randomx_jit_wx": mh.JITIsSecure(),
		// For the progress indicator in the wallet: how many peers, how far the chain reaches
		// according to them and the height the UTXO set is finished up to. `best_known` comes
		// from strangers — it is not used for consensus, only so that "40 %" can be said.
		"peers":          peers,
		"best_known":     bestKnown,
		"applied_height": applied,
		"synced":         applied >= height && (bestKnown == 0 || height >= bestKnown),
		// settlement_check: "all" — the node verifies the PoW of every share the blocks settle;
		// "sampled" — a light node, it checks a few at random and takes the rest on trust. An
		// exchange, a pool and a miner want "all".
		"settlement_check": settlementCheck(s.backend.ChecksEveryShare()),
	}, nil
}

func settlementCheck(all bool) string {
	if all {
		return "all"
	}
	return "sampled"
}

// getFinality is what an exchange looks at before crediting a deposit. `finalized`, `height` and
// `block` are what it needs; `quorum` shows how the block on its way is doing.
func (s *Server) getFinality() (any, error) {
	height, id, ok := s.backend.Finality()
	out := map[string]any{"finalized": ok}
	if ok {
		out["height"] = height
		out["block"] = hex.EncodeToString(id[:])
	}
	st := s.backend.FinalityStatus()
	if !st.Enabled {
		out["enabled"] = false
		return out, nil
	}
	out["enabled"] = true
	out["depth"] = chain.FinalityDepth
	out["window"] = st.Window
	if st.Active > 0 {
		out["active"] = st.Active
	}
	out["threshold"] = fmt.Sprintf("more than %d/%d", chain.FinalityNum, chain.FinalityDen)
	if st.Key != nil {
		out["key"] = hex.EncodeToString(st.Key[:])
	}
	if st.Caught > 0 {
		out["caught_keys"] = st.Caught
	}
	q := map[string]any{"height": st.PendingHeight, "members": st.Members}
	if st.Members > 0 {
		q["block"] = hex.EncodeToString(st.PendingBlock[:])
		q["signed_permille"] = permille(st.Signed, st.Total)
		q["own_permille"] = permille(st.Own, st.Total)
	}
	out["quorum"] = q
	return out, nil
}

func permille(part, total uint64) uint64 {
	if total == 0 {
		return 0
	}
	return part * 1000 / total
}

func (s *Server) getBlock(params json.RawMessage) (any, error) {
	var p struct {
		ID     string  `json:"id"`
		Height *uint64 `json:"height"`
	}
	if len(params) > 0 {
		if err := decodeParams(params, &p); err != nil {
			return nil, err
		}
	}
	var id [32]byte
	switch {
	case p.ID != "":
		raw, err := hex.DecodeString(p.ID)
		if err != nil || len(raw) != 32 {
			return nil, errors.New("id has to be 32 bytes in hex")
		}
		copy(id[:], raw)
	case p.Height != nil:
		got, ok := s.backend.HeaderAt(*p.Height)
		if !ok {
			return nil, errors.New("we have nothing at that height")
		}
		id = got
	default:
		return nil, errors.New("id or height is required")
	}

	b, ok := s.backend.Block(id)
	if !ok {
		return nil, errors.New("we do not have the block body")
	}
	txs := make([]string, 0, len(b.Txs))
	for _, tx := range b.Txs {
		txid := tx.TxID()
		txs = append(txs, hex.EncodeToString(txid[:]))
	}
	out := map[string]any{
		"id":          hex.EncodeToString(id[:]),
		"height":      b.Header.Height,
		"lane":        b.Header.Lane.String(),
		"time":        b.Header.Time,
		"bits":        fmt.Sprintf("%08x", b.Header.Bits),
		"version":     b.Header.Version,
		"merkle_root": hex.EncodeToString(b.Header.MerkleRoot[:]),
		"nonce":       b.Header.Nonce,
		"txs":         txs,
		"size":        len(b.Serialize()),
	}
	// The genesis has no predecessor; the explorer would offer a zero ID as a link into nothing.
	if b.Header.Height > 0 {
		out["prev_block"] = hex.EncodeToString(b.Header.PrevBlock[:])
	}
	// The share commitment: who closed the block, under its PoW (the Sharepool spec §2.1).
	if b.Share.Payout != ([32]byte{}) {
		if a, err := s.backend.AddressOf(chain.ScriptVersionBasic, b.Share.Payout[:]); err == nil {
			out["payout"] = a
		}
	}
	if key, ok := b.FinalityKey(); ok {
		out["finality_key"] = hex.EncodeToString(key[:])
	}
	out["payout_threshold"] = b.Share.Threshold
	out["share_weight"] = b.Share.Weight
	// Who the block pays: the shares of its settlement body by miner (the Sharepool spec §3.1).
	if tally := b.Settlement.Tally(); len(tally) > 0 {
		out["settlement"] = s.tallyOut(tally)
	}
	// The block's work — what it contributed to the chainwork. Not "difficulty" as a multiple of
	// some historical value; with two lanes with their own retarget that says nothing.
	if w, err := chain.Work(b.Header.Bits); err == nil {
		out["work"] = w.String()
	}
	// Confirmations and the link to the next block are computed against the active branch: a
	// block from a disconnected branch has zero confirmations and no successor.
	_, tipHeight, _ := s.backend.Tip()
	h := uint64(b.Header.Height)
	if active, ok := s.backend.HeaderAt(h); ok && active == id {
		out["confirmations"] = tipHeight - h + 1
		if next, ok := s.backend.HeaderAt(h + 1); ok {
			out["next_block"] = hex.EncodeToString(next[:])
		}
	} else {
		out["confirmations"] = 0
		out["stale"] = true
	}
	return out, nil
}

func (s *Server) getRawMempool() (any, error) {
	ids := s.backend.MempoolIDs()
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, hex.EncodeToString(id[:]))
	}
	return out, nil
}

func (s *Server) sendRawTransaction(params json.RawMessage) (any, error) {
	var p struct {
		Hex string `json:"hex"`
	}
	if err := decodeParams(params, &p); err != nil {
		return nil, err
	}
	raw, err := hex.DecodeString(p.Hex)
	if err != nil {
		return nil, errors.New("the transaction has to be in hex")
	}
	tx, used, err := chain.DeserializeTx(raw)
	if err != nil {
		return nil, err
	}
	if used != len(raw) {
		return nil, errors.New("there are extra bytes after the transaction")
	}
	if err := s.backend.SubmitTx(tx); err != nil {
		return nil, err
	}
	id := tx.TxID()
	return hex.EncodeToString(id[:]), nil
}

// submitBlock accepts a mined block (hex). It returns the ID and the height; an error when the
// block did not pass — on "stale" the miner should take a new template right away.
func (s *Server) submitBlock(params json.RawMessage) (any, error) {
	var p struct {
		Hex string `json:"hex"`
	}
	if err := decodeParams(params, &p); err != nil {
		return nil, err
	}
	raw, err := hex.DecodeString(p.Hex)
	if err != nil {
		return nil, errors.New("the block has to be in hex")
	}
	blk, err := chain.DeserializeBlock(raw)
	if err != nil {
		return nil, err
	}
	if err := s.backend.SubmitBlock(blk); err != nil {
		return nil, err
	}
	id := blk.Header.ID()
	return map[string]any{"id": hex.EncodeToString(id[:]), "height": blk.Header.Height}, nil
}

// submitShare takes a share in its full form (chain.ShareBytes, hex).
func (s *Server) submitShare(params json.RawMessage) (any, error) {
	var p struct {
		Hex string `json:"hex"`
	}
	if err := decodeParams(params, &p); err != nil {
		return nil, err
	}
	raw, err := hex.DecodeString(p.Hex)
	if err != nil {
		return nil, errors.New("the share has to be in hex")
	}
	sh, err := chain.DeserializeShare(raw)
	if err != nil {
		return nil, err
	}
	block, err := s.backend.SubmitShare(sh)
	if err != nil {
		return nil, err
	}
	id := sh.ID()
	return map[string]any{"id": hex.EncodeToString(id[:]), "units": sh.Commit.Units(), "block": block}, nil
}

// MaxGenerate is the cap on blocks per generatetoaddress.
const MaxGenerate = 1000

func (s *Server) generate(params json.RawMessage) (any, error) {
	var p struct {
		Count   int    `json:"nblocks"`
		Address string `json:"address"`
	}
	if err := decodeParams(params, &p); err != nil {
		return nil, err
	}
	if p.Count <= 0 || p.Count > MaxGenerate {
		return nil, fmt.Errorf("nblocks has to be 1–%d", MaxGenerate)
	}
	ids, err := s.backend.Generate(p.Count, p.Address)
	if err != nil {
		return nil, err
	}
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = hex.EncodeToString(id[:])
	}
	return out, nil
}

// MaxRawBlocks is the cap on blocks per getrawblocks request.
const MaxRawBlocks = 100

// getRawBlocks returns the raw blocks of the active branch from height `from`. This is what the
// wallet connects to: it decodes the blocks with the same code as the node
// (chain.DeserializeBlock) and finds its own coins itself — the node does not learn which
// addresses it cares about.
func (s *Server) getRawBlocks(params json.RawMessage) (any, error) {
	var p struct {
		From  uint64 `json:"from"`
		Count int    `json:"count"`
	}
	if len(params) > 0 {
		if err := decodeParams(params, &p); err != nil {
			return nil, err
		}
	}
	if p.Count <= 0 || p.Count > MaxRawBlocks {
		p.Count = MaxRawBlocks
	}
	_, tipHeight, _ := s.backend.Tip()
	out := make([]map[string]any, 0, p.Count)
	for h := p.From; h <= tipHeight && len(out) < p.Count; h++ {
		id, ok := s.backend.HeaderAt(h)
		if !ok {
			break
		}
		b, ok := s.backend.Block(id)
		if !ok {
			break // we do not have the body yet (the node is catching up) — the wallet will ask later
		}
		out = append(out, map[string]any{
			"height": h,
			"id":     hex.EncodeToString(id[:]),
			"hex":    hex.EncodeToString(b.Serialize()),
		})
	}
	return out, nil
}

func (s *Server) getTemplate(params json.RawMessage) (any, error) {
	var p struct {
		Lane string `json:"lane"`
	}
	if len(params) > 0 {
		if err := decodeParams(params, &p); err != nil {
			return nil, err
		}
	}
	lane := mh.LaneG
	switch p.Lane {
	case "", "G":
		lane = mh.LaneG
	case "C":
		lane = mh.LaneC
	default:
		return nil, fmt.Errorf("unknown lane %q", p.Lane)
	}
	return s.backend.Template(lane)
}

// getHashrate returns the hashrate over time for each lane: a sliding window of work divided by
// time.
//
// The values are **strings** (hashes per second), not numbers — the network hashrate easily grows
// past 2⁵³ and a JSON number would be rounded in JavaScript. The same reason as for chainwork.
func (s *Server) getHashrate(params json.RawMessage) (any, error) {
	var p struct {
		Blocks int `json:"blocks"`
		Window int `json:"window"`
		Points int `json:"points"`
	}
	p.Blocks, p.Window, p.Points = 2000, 12, 120
	if len(params) > 0 {
		if err := decodeParams(params, &p); err != nil {
			return nil, err
		}
	}
	series := s.backend.HashrateSeries(p.Blocks, p.Window, p.Points)
	lanes := make([]map[string]any, 0, len(series.Lanes))
	for _, lane := range mh.Lanes {
		pts := series.Lanes[lane]
		out := make([]map[string]any, 0, len(pts))
		for _, pt := range pts {
			out = append(out, map[string]any{"t": pt.Time, "hs": pt.Hashrate.String(), "h": pt.Height})
		}
		lanes = append(lanes, map[string]any{"lane": lane.String(), "points": out})
	}
	return map[string]any{"window_blocks": series.WindowBlocks, "lanes": lanes}, nil
}

// getRawTransaction returns a transaction together with what it spends.
//
// The inputs are enriched with the address and the value of the previous output — that is the
// equivalent of what Bitcoin added as `getblock` verbosity 3: without it the explorer has to
// fetch another transaction for every input, and one block turns into hundreds of calls.
//
// **The amounts are integers in cogs**, not decimals. Bitcoin sends them as decimals and that is
// a source of rounding bugs; we ban floats in consensus (project rule 2: no floats in consensus) and there is no sense
// in breaking that exactly at the boundary where foreign software reads the values.
func (s *Server) getRawTransaction(params json.RawMessage) (any, error) {
	var p struct {
		TxID string `json:"txid"`
	}
	if len(params) > 0 {
		if err := decodeParams(params, &p); err != nil {
			return nil, err
		}
	}
	raw, err := hex.DecodeString(p.TxID)
	if err != nil || len(raw) != 32 {
		return nil, errors.New("txid has to be 32 bytes in hex")
	}
	var txid [32]byte
	copy(txid[:], raw)

	tx, blk, idx, err := s.backend.Tx(txid)
	if err != nil {
		return nil, err
	}

	_, tipHeight, _ := s.backend.Tip()
	height := uint64(blk.Header.Height)
	confirmations := uint64(0)
	if tipHeight >= height {
		confirmations = tipHeight - height + 1
	}

	// From the transaction, not from the position in the block. On the active branch the coinbase
	// is always first, but deriving that from the index means trusting the caller about the order.
	coinbase := tx.IsCoinbase()
	vin := make([]any, 0, len(tx.Inputs))
	var valueIn int64
	for _, in := range tx.Inputs {
		if coinbase {
			vin = append(vin, map[string]any{"coinbase": true})
			continue
		}
		e := map[string]any{
			"txid": hex.EncodeToString(in.PrevTxID[:]),
			"vout": in.PrevIndex,
		}
		// The previous output — its value and address. When it cannot be looked up (the index is
		// not on), the field is left out; lying with a zero would be worse than a missing number.
		if prev, _, _, err := s.backend.Tx(in.PrevTxID); err == nil &&
			int(in.PrevIndex) < len(prev.Outputs) {
			out := prev.Outputs[in.PrevIndex]
			e["value_cogs"] = out.Value
			valueIn += out.Value
			if a, err := s.backend.AddressOf(out.ScriptVersion, out.Script); err == nil {
				e["address"] = a
			}
		}
		vin = append(vin, e)
	}

	vout := make([]any, 0, len(tx.Outputs))
	var valueOut int64
	for i, out := range tx.Outputs {
		valueOut += out.Value
		e := map[string]any{
			"n":              i,
			"value_cogs":     out.Value,
			"script_version": out.ScriptVersion,
			"script":         hex.EncodeToString(out.Script),
		}
		if a, err := s.backend.AddressOf(out.ScriptVersion, out.Script); err == nil {
			e["address"] = a
		}
		vout = append(vout, e)
	}

	blockID := blk.Header.ID()
	res := map[string]any{
		"txid":           hex.EncodeToString(txid[:]),
		"block":          hex.EncodeToString(blockID[:]),
		"height":         height,
		"index":          idx,
		"time":           blk.Header.Time,
		"lane":           blk.Header.Lane.String(),
		"confirmations":  confirmations,
		"coinbase":       coinbase,
		"vin":            vin,
		"vout":           vout,
		"value_out_cogs": valueOut,
		"size":           len(tx.Serialize()),
	}
	if !coinbase {
		res["value_in_cogs"] = valueIn
		res["fee_cogs"] = valueIn - valueOut
	}
	return res, nil
}

// getAddress returns a summary of an address and its recent history.
//
// The direction filter works on the **net amount per transaction**, not on "the address is among
// the outputs". Change comes back to the same address, so a naive "in" filter does not filter at
// all and "out" duplicates the row for every spent output.
func (s *Server) getAddress(params json.RawMessage) (any, error) {
	var p struct {
		Address string `json:"address"`
		Before  uint64 `json:"before"`
		Limit   int    `json:"limit"`
	}
	if len(params) > 0 {
		if err := decodeParams(params, &p); err != nil {
			return nil, err
		}
	}
	if p.Address == "" {
		return nil, errors.New("an address is required")
	}
	sum, entries, err := s.backend.Address(p.Address, p.Before, p.Limit)
	if err != nil {
		return nil, err
	}

	// Group the movements by transaction and compute the net amount.
	type row struct {
		TxID      string `json:"txid"`
		Height    uint64 `json:"height"`
		NetCogs   int64  `json:"net_cogs"`
		InCogs    int64  `json:"in_cogs"`
		OutCogs   int64  `json:"out_cogs"`
		Direction string `json:"direction"`
	}
	order := make([][32]byte, 0, len(entries))
	byTx := make(map[[32]byte]*row, len(entries))
	for _, e := range entries {
		r := byTx[e.TxID]
		if r == nil {
			r = &row{TxID: hex.EncodeToString(e.TxID[:]), Height: e.Height}
			byTx[e.TxID] = r
			order = append(order, e.TxID)
		}
		if e.Received {
			r.InCogs += e.Value
		} else {
			r.OutCogs += e.Value
		}
	}
	txs := make([]row, 0, len(order))
	for _, id := range order {
		r := byTx[id]
		r.NetCogs = r.InCogs - r.OutCogs
		switch {
		case r.NetCogs > 0:
			r.Direction = "in"
		case r.NetCogs < 0:
			r.Direction = "out"
		default:
			r.Direction = "self"
		}
		txs = append(txs, *r)
	}

	return map[string]any{
		"address":       p.Address,
		"balance_cogs":  sum.Balance(),
		"received_cogs": sum.Received,
		"sent_cogs":     sum.Sent,
		"tx_count":      sum.TxCount,
		"txs":           txs,
	}, nil
}

// getRichList returns the addresses with the highest balance.
//
// On a small coin this is not whale watching but a tool of trust: it shows whether the
// distribution matches what the project claims. That is why the share of the supply belongs
// there too — and it has to add up to 100 %, not more (iquidus has an issue "Richlist pie chart
// sums to 126%").
func (s *Server) getRichList(params json.RawMessage) (any, error) {
	var p struct {
		Limit int `json:"limit"`
	}
	if len(params) > 0 {
		if err := decodeParams(params, &p); err != nil {
			return nil, err
		}
	}
	entries, supply, err := s.backend.RichList(p.Limit)
	if err != nil {
		return nil, err
	}
	rows := make([]any, 0, len(entries))
	for i, e := range entries {
		row := map[string]any{
			"rank":         i + 1,
			"balance_cogs": e.Balance,
		}
		if a, err := s.backend.AddressOf(e.ScriptVersion, e.Script); err == nil {
			row["address"] = a
		}
		// The share is sent as whole per mille, not a float — a percentage with a decimal point is
		// exactly where a rounding error shows up in a foreign client.
		if supply > 0 {
			row["share_permille"] = e.Balance * 1000 / supply
		}
		rows = append(rows, row)
	}
	return map[string]any{
		"supply_cogs": supply,
		"addresses":   rows,
	}, nil
}

// getBlocks returns a summary of the blocks in a range of heights, from the highest down.
//
// The list of recent blocks is on every page of the explorer. Through getblockhash+getblock it
// would be twice as many calls as there are rows — and again on every page load. The summary
// deliberately carries no transactions, only their count: whoever wants them goes to the block
// detail.

func (s *Server) getBlocks(params json.RawMessage) (any, error) {
	var p struct {
		From  *uint64 `json:"from"`
		Count int     `json:"count"`
	}
	if len(params) > 0 {
		if err := decodeParams(params, &p); err != nil {
			return nil, err
		}
	}
	_, tipHeight, _ := s.backend.Tip()
	from := tipHeight
	if p.From != nil {
		from = *p.From
	}
	if from > tipHeight {
		from = tipHeight
	}
	switch {
	case p.Count <= 0:
		p.Count = 20
	case p.Count > MaxBlockListCount:
		p.Count = MaxBlockListCount
	}

	out := make([]any, 0, p.Count)
	for h := int64(from); h >= 0 && len(out) < p.Count; h-- {
		id, ok := s.backend.HeaderAt(uint64(h))
		if !ok {
			break
		}
		b, haveBody := s.backend.Block(id)
		if !haveBody {
			// A header without a body is a normal state for a moment after every block, and a
			// lasting one when a body is not arriving. The row is still listed — **the height
			// must never disappear from the list**: a list that closes the gap claims the two
			// neighbouring blocks follow one another, and that can make a perfectly valid
			// chain look like it has four blocks of one lane in a row. What the header does
			// not know (the transactions and the size) is simply left out.
			h, ok := s.backend.Header(id)
			if !ok {
				break
			}
			out = append(out, map[string]any{
				"id":     hex.EncodeToString(id[:]),
				"height": h.Height,
				"lane":   h.Lane.String(),
				"time":   h.Time,
				"bits":   fmt.Sprintf("%08x", h.Bits),
				"body":   false,
			})
			continue
		}
		var outValue int64
		for _, tx := range b.Txs {
			for _, o := range tx.Outputs {
				outValue += o.Value
			}
		}
		row := map[string]any{
			"id":             hex.EncodeToString(id[:]),
			"height":         b.Header.Height,
			"lane":           b.Header.Lane.String(),
			"time":           b.Header.Time,
			"bits":           fmt.Sprintf("%08x", b.Header.Bits),
			"tx_count":       len(b.Txs),
			"size":           len(b.Serialize()),
			"value_out_cogs": outValue,
			"body":           true,
			"shares":         len(b.Settlement.Shares),
		}
		if b.Share.Payout != ([32]byte{}) {
			if a, err := s.backend.AddressOf(chain.ScriptVersionBasic, b.Share.Payout[:]); err == nil {
				row["payout"] = a
			}
		}
		out = append(out, row)
	}
	return map[string]any{"height": tipHeight, "blocks": out}, nil
}

// MaxTallyMiners caps the miners a share summary lists; the totals still count every one.
const MaxTallyMiners = 500

// tallyOut is a share summary as the RPC answers it: the totals, and the miners by units, the most
// first, a tie by address.
func (s *Server) tallyOut(tally map[[32]byte]chain.ShareTally) map[string]any {
	type row struct {
		addr string
		t    chain.ShareTally
	}
	rows := make([]row, 0, len(tally))
	var shares int
	var units uint64
	for p, t := range tally {
		shares += t.Shares
		units += t.Units
		a, err := s.backend.AddressOf(chain.ScriptVersionBasic, p[:])
		if err != nil {
			a = hex.EncodeToString(p[:])
		}
		rows = append(rows, row{a, t})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].t.Units != rows[j].t.Units {
			return rows[i].t.Units > rows[j].t.Units
		}
		return rows[i].addr < rows[j].addr
	})
	miners := make([]map[string]any, 0, min(len(rows), MaxTallyMiners))
	for _, r := range rows[:min(len(rows), MaxTallyMiners)] {
		miners = append(miners, map[string]any{"address": r.addr, "shares": r.t.Shares, "units": r.t.Units})
	}
	return map[string]any{"shares": shares, "units": units, "miner_count": len(rows), "miners": miners}
}

// getSharePool sums the node's share pool by miner: what the next blocks may settle.
func (s *Server) getSharePool() (any, error) {
	full, tally := s.backend.SharePool()
	out := s.tallyOut(tally)
	out["takes_shares"] = full
	return out, nil
}

// MaxCredits caps the entries getcredits lists; `count` still counts every one.
const MaxCredits = 10_000

// getCredits lists the credit state: what each miner has earned toward its payout. The entries
// come by the last block that settled a share of theirs, the most recent first, so the miners who
// mine now are at the top, a tie by amount and then by address.
func (s *Server) getCredits(params json.RawMessage) (any, error) {
	var p struct {
		Limit   int    `json:"limit"`
		Address string `json:"address"`
	}
	if len(params) > 0 {
		if err := decodeParams(params, &p); err != nil {
			return nil, err
		}
	}
	if p.Limit <= 0 || p.Limit > MaxCredits {
		p.Limit = MaxCredits
	}
	// With an address only that address's credit comes back (none if it has nothing waiting), so a
	// miner finds its own credit however many others there are.
	p.Address = strings.ToLower(strings.TrimSpace(p.Address))
	pot, height, count, credits, err := s.backend.Credits(p.Address, p.Limit)
	if err != nil {
		return nil, err
	}
	type row struct {
		addr string
		c    chain.Credit
	}
	rows := make([]row, 0, len(credits))
	for _, e := range credits {
		a, err := s.backend.AddressOf(chain.ScriptVersionBasic, e.Payout[:])
		if err != nil {
			a = hex.EncodeToString(e.Payout[:])
		}
		rows = append(rows, row{a, e.Credit})
	}
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i].c, rows[j].c
		if a.LastShare != b.LastShare {
			return a.LastShare > b.LastShare
		}
		if a.Amount != b.Amount {
			return a.Amount > b.Amount
		}
		return rows[i].addr < rows[j].addr
	})
	out := make([]map[string]any, 0, min(len(rows), p.Limit))
	for _, r := range rows[:min(len(rows), p.Limit)] {
		out = append(out, map[string]any{
			"address":     r.addr,
			"amount_cogs": r.c.Amount,
			"need_cogs":   chain.PayoutNeed(r.c.Threshold),
			"last_share":  r.c.LastShare,
			"paid_at":     r.c.PaidAt,
			"ready_at":    r.c.ReadyAt,
		})
	}
	return map[string]any{"height": height, "pot_cogs": pot, "count": count, "credits": out}, nil
}

// getShareGeo is the country of the shares the node took first: the country of the address each
// came in from (a stratum device, or the peer that relayed it), the most first. No address is
// kept anywhere.
func (s *Server) getShareGeo() (any, error) {
	on, minutes, window, seen, unknown, seenUnknown := s.backend.ShareGeo()
	rows := make([]map[string]any, 0, len(seen))
	for cc, n := range seen {
		rows = append(rows, map[string]any{"country": cc, "shares": window[cc], "seen": n})
	}
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i]["shares"].(int), rows[j]["shares"].(int)
		if a != b {
			return a > b
		}
		return rows[i]["country"].(string) < rows[j]["country"].(string)
	})
	return map[string]any{"enabled": on, "minutes": minutes, "countries": rows, "unknown": unknown, "seen_unknown": seenUnknown}, nil
}

// getLaneStats returns the state of the individual lanes.
//
// `getdifficulty` stays **scalar** and does not change here. DigiByte turned it into an object
// with a value per algorithm and generic tools broke on it — cryptoID has shown `Difficulty ---`
// for DGB ever since and its block list does not even have a column for the algorithm. Per-lane
// data belongs in a method of its own, not in a reshaped old one.
// getForks returns the consensus rules added after the genesis: the height each starts at on
// this network and how many of the most recent blocks say their miner is ready for it.
func (s *Server) getForks() (any, error) {
	st := s.backend.ForkStatus()

	blocks := make(map[string]int, len(mh.Lanes))
	for _, lane := range mh.Lanes {
		blocks[lane.String()] = st.Blocks[lane]
	}
	forks := make([]any, 0, len(st.Forks))
	for _, f := range st.Forks {
		signalling := make(map[string]int, len(mh.Lanes))
		for _, lane := range mh.Lanes {
			signalling[lane.String()] = f.Signalling[lane]
		}
		e := map[string]any{
			"name":       f.Fork.String(),
			"bit":        uint8(f.Fork),
			"known":      f.Known,
			"active":     f.Active,
			"signalling": signalling,
		}
		if f.Scheduled {
			e["height"] = f.Height
		}
		forks = append(forks, e)
	}
	return map[string]any{
		"height":        st.TipHeight,
		"window":        st.Window,
		"window_blocks": blocks,
		"forks":         forks,
	}, nil
}

func (s *Server) getLaneStats() (any, error) {
	st := s.backend.LaneStats()

	lanes := make([]any, 0, len(st.Lanes))
	for _, l := range st.Lanes {
		share := make(map[string]any, len(st.Windows))
		for i, w := range st.Windows {
			n := l.BlocksInWindow[i]
			// The denominator is how many blocks are really in the window, not the nominal window
			// size. A young chain does not have 120 blocks and a share against one hundred and
			// twenty would turn half the network into 14 %.
			fill := st.WindowFill[i]
			e := map[string]any{"blocks": n, "of_blocks": fill}
			// The share in whole per mille, not a percentage with a decimal point.
			if fill > 0 {
				e["share_permille"] = n * 1000 / fill
			}
			share[fmt.Sprintf("last_%d", w)] = e
		}
		e := map[string]any{
			"lane":              l.Lane.String(),
			"bits":              fmt.Sprintf("%08x", l.Bits),
			"share":             share,
			"target_spacing_s":  int64(chain.LaneBlockTime.Seconds()),
			"last_block_height": l.LastHeight,
			"last_block_time":   l.LastTime,
		}
		if l.WorkPerBlock != nil {
			e["work_per_block"] = l.WorkPerBlock.String()
		}
		if l.AvgSpacingSec > 0 {
			// Whole seconds — a fraction of a second at a 60 s spacing interests nobody and a
			// decimal is needless room for misunderstanding.
			e["avg_spacing_s"] = int64(l.AvgSpacingSec + 0.5)
		}
		if l.HashrateEst != nil {
			e["hashrate_est"] = l.HashrateEst.String()
		}
		lanes = append(lanes, e)
	}

	return map[string]any{
		"height":  st.TipHeight,
		"windows": st.Windows,
		"lanes":   lanes,
		// The run of the same lane at the tip. Consensus allows at most max_same_lane_run; a higher
		// value means something is wrong — and that should be visible in the explorer.
		"run": map[string]any{
			"lane":              st.RunLane.String(),
			"length":            st.RunLength,
			"max_same_lane_run": chain.MaxSameLaneRun,
		},
	}, nil
}

// Positional parameters.
//
// Our native shape is a named object — it is readable and has no order to forget. But
// **bitcoin-style tools send positional arrays exclusively**: btc-rpc-explorer, eiquidus and
// ElectrumX. If we only took objects, three out of four clients we might one day want to support
// would bounce off us.
//
// So the array is rewritten into an object **before** the branching and the handlers know nothing
// about it. It is one extra place, not duplicated processing in every method.
var positionalNames = map[string][]string{
	"getblock":           {"id"},
	"getblockhash":       {"height"},
	"gettxout":           {"txid", "vout"},
	"getrawtransaction":  {"txid"},
	"getaddress":         {"address", "before", "limit"},
	"getrichlist":        {"limit"},
	"getcredits":         {"limit", "address"},
	"sendrawtransaction": {"hex"},
	"gettemplate":        {"lane"},
	"getrawblocks":       {"from", "count"},
	"gethashrate":        {"blocks", "window", "points"},
	"generatetoaddress":  {"nblocks", "address"},
	"submitblock":        {"hex"},
	"submitshare":        {"hex"},
}

// normalizeParams rewrites a positional array into a named object. An object and missing
// parameters pass through unchanged.
func normalizeParams(method string, raw json.RawMessage) (json.RawMessage, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return raw, nil
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(trimmed, &arr); err != nil {
		return nil, err
	}
	if len(arr) == 0 {
		return nil, nil // `params: []` is the same as no parameters
	}
	names, ok := positionalNames[method]
	if !ok {
		return nil, fmt.Errorf("method %s does not take positional parameters", method)
	}
	if len(arr) > len(names) {
		return nil, fmt.Errorf("method %s takes at most %d parameters, got %d",
			method, len(names), len(arr))
	}
	obj := make(map[string]json.RawMessage, len(arr))
	for i, v := range arr {
		obj[names[i]] = v
	}
	return json.Marshal(obj)
}

// Small methods practically every tool wants from the node. They are here so that neither the
// explorer nor an exchange has to query indirectly through `getinfo` and parse what they need out of it.

func (s *Server) getBlockCount() (any, error) {
	_, height, _ := s.backend.Tip()
	return height, nil
}

func (s *Server) getBestBlockHash() (any, error) {
	_, _, id := s.backend.Tip()
	return hex.EncodeToString(id[:]), nil
}

func (s *Server) getBlockHash(params json.RawMessage) (any, error) {
	var p struct {
		Height uint64 `json:"height"`
	}
	if len(params) > 0 {
		if err := decodeParams(params, &p); err != nil {
			return nil, err
		}
	}
	id, ok := s.backend.HeaderAt(p.Height)
	if !ok {
		return nil, fmt.Errorf("we have no block at height %d", p.Height)
	}
	return hex.EncodeToString(id[:]), nil
}

// getTxOut returns an unspent output. A spent output is `null`, not an error — exactly as Bitcoin
// does it and exactly how it is used to check whether an output is still available.
func (s *Server) getTxOut(params json.RawMessage) (any, error) {
	var p struct {
		TxID string `json:"txid"`
		Vout uint32 `json:"vout"`
	}
	if len(params) > 0 {
		if err := decodeParams(params, &p); err != nil {
			return nil, err
		}
	}
	raw, err := hex.DecodeString(p.TxID)
	if err != nil || len(raw) != 32 {
		return nil, errors.New("txid has to be 32 bytes in hex")
	}
	var txid [32]byte
	copy(txid[:], raw)

	coin, ok := s.backend.Coin(chain.Outpoint{TxID: txid, Index: p.Vout})
	if !ok {
		return nil, nil
	}
	_, tipHeight, _ := s.backend.Tip()
	confirmations := uint64(0)
	if tipHeight >= coin.Height {
		confirmations = tipHeight - coin.Height + 1
	}
	out := map[string]any{
		"value_cogs":     coin.Value,
		"script_version": coin.ScriptVersion,
		"script":         hex.EncodeToString(coin.Script),
		"height":         coin.Height,
		"coinbase":       coin.Coinbase,
		"confirmations":  confirmations,
	}
	if a, err := s.backend.AddressOf(coin.ScriptVersion, coin.Script); err == nil {
		out["address"] = a
	}
	return out, nil
}
