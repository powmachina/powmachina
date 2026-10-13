package rpc

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"powmachina/internal/address"
	"powmachina/internal/chain"
	"powmachina/internal/mh"
	"powmachina/internal/store"
)

// fakeBackend is a minimal node for the RPC tests.
type fakeBackend struct {
	attestations []chain.Attestation
	height       uint64
	submitted    []*chain.Tx
	submitErr    error
	finalized    bool
	txs          map[[32]byte]txEntry
	addr         map[string]addrEntry
	rich         []store.RichEntry
	supply       int64
	lanes        chain.LaneStats
	forks        chain.ForkStatus
	coins        map[chain.Outpoint]chain.Coin
	// sync state for getinfo; zeros mean "synced" (see SyncState)
	syncPeers   int
	syncBest    uint64
	syncApplied uint64
}

type addrEntry struct {
	sum  store.AddrSummary
	hist []store.AddrEntry
}

type txEntry struct {
	tx  *chain.Tx
	blk *chain.Block
	idx uint32
}

func (f *fakeBackend) Params() chain.NetParams { return chain.RegTest }
func (f *fakeBackend) NodeName() string        { return "test-node" }

// txs is a small graph of transactions. Empty = a node without -txindex, exactly as in production.
func (f *fakeBackend) Tx(txid [32]byte) (*chain.Tx, *chain.Block, uint32, error) {
	if e, ok := f.txs[txid]; ok {
		return e.tx, e.blk, e.idx, nil
	}
	if f.txs == nil {
		return nil, nil, 0, errors.New("the transaction index is not enabled")
	}
	return nil, nil, 0, errors.New("the transaction does not exist")
}

func (f *fakeBackend) AddressOf(scriptVersion uint8, script []byte) (string, error) {
	return address.Encode(chain.RegTest.Bech32HRP, scriptVersion, script)
}

// addr is a fake address index: empty = a node without -addrindex.
func (f *fakeBackend) Address(addr string, before uint64, limit int) (store.AddrSummary, []store.AddrEntry, error) {
	if f.addr == nil {
		return store.AddrSummary{}, nil, errors.New("the address index is not enabled")
	}
	e, ok := f.addr[addr]
	if !ok {
		return store.AddrSummary{}, nil, nil
	}
	return e.sum, e.hist, nil
}

func (f *fakeBackend) RichList(limit int) ([]store.RichEntry, int64, error) {
	if f.rich == nil {
		return nil, 0, errors.New("the address index is not enabled")
	}
	return f.rich, f.supply, nil
}

func (f *fakeBackend) LaneStats() chain.LaneStats { return f.lanes }

func (f *fakeBackend) ForkStatus() chain.ForkStatus { return f.forks }

func (f *fakeBackend) Coin(op chain.Outpoint) (chain.Coin, bool) {
	c, ok := f.coins[op]
	return c, ok
}
func (f *fakeBackend) Tip() (*chain.Header, uint64, [32]byte) {
	h := &chain.Header{Height: uint32(f.height), Time: 1_700_000_000, Lane: mh.LaneG}
	return h, f.height, h.ID()
}
func (f *fakeBackend) ChainWork() *big.Int                 { return big.NewInt(12345) }
func (f *fakeBackend) UTXOCount() int                      { return 7 }
func (f *fakeBackend) MempoolStats() (int, int)            { return len(f.submitted), 42 }
func (f *fakeBackend) MempoolIDs() [][32]byte              { return nil }
func (f *fakeBackend) HeaderAt(uint64) ([32]byte, bool)    { return [32]byte{}, false }
func (f *fakeBackend) Block([32]byte) (*chain.Block, bool) { return nil, false }
func (f *fakeBackend) Header([32]byte) (*chain.Header, bool) {
	return nil, false
}
func (f *fakeBackend) Finality() (uint64, [32]byte, bool) {
	if !f.finalized {
		return 0, [32]byte{}, false
	}
	return f.height - 20, [32]byte{9}, true
}
func (f *fakeBackend) SubmitTx(tx *chain.Tx) error {
	if f.submitErr != nil {
		return f.submitErr
	}
	f.submitted = append(f.submitted, tx)
	return nil
}
func (f *fakeBackend) AllowedLanes() []mh.Lane         { return []mh.Lane{mh.LaneC, mh.LaneG} }
func (f *fakeBackend) SubmitBlock(*chain.Block) error  { return errors.New("no") }
func (f *fakeBackend) TrustedWork() bool               { return true }
func (f *fakeBackend) ClockSkew() (time.Duration, int) { return 0, 0 }

// SyncState: two peers, the chain reaches the tip according to them and the UTXO set is
// finished — that is, a synced node. Tests that want the opposite set the fields themselves.
func (f *fakeBackend) SyncState() (int, uint64, uint64) {
	_, h, _ := f.Tip()
	if f.syncPeers == 0 && f.syncBest == 0 && f.syncApplied == 0 {
		return 2, h, h
	}
	return f.syncPeers, f.syncBest, f.syncApplied
}

// SubmitAttestation remembers what arrived — the tests check it.
func (f *fakeBackend) SubmitAttestation(att chain.Attestation) error {
	f.attestations = append(f.attestations, att)
	return nil
}
func (f *fakeBackend) StratumInfo() (any, error) { return nil, errors.New("no") }
func (f *fakeBackend) HashrateSeries(_, _, _ int) chain.HashrateSeries {
	return chain.HashrateSeries{WindowBlocks: 12, Lanes: map[mh.Lane][]chain.HashratePoint{
		mh.LaneC: {{Time: 1_800_000_000, Hashrate: big.NewInt(12_345)}},
		mh.LaneG: {{Time: 1_800_000_000, Hashrate: big.NewInt(890_000)}},
	}}
}
func (f *fakeBackend) Generate(int, string) ([][32]byte, error) {
	return nil, errors.New("regtest only")
}
func (f *fakeBackend) NewAddress() (string, error) {
	return "aa" + hex.EncodeToString(make([]byte, 31)), nil
}
func (f *fakeBackend) Balance() int64 { return 123456 }
func (f *fakeBackend) SendToAddress(string, int64) (string, error) {
	return hex.EncodeToString(make([]byte, 32)), nil
}
func (f *fakeBackend) WalletInfo() (string, uint32, string) { return "/tmp/w", 3, "m/44'/9797'/0'/0" }
func (f *fakeBackend) DumpSeed() string                     { return "00112233" }
func (f *fakeBackend) Template(lane mh.Lane) (*Template, error) {
	return &Template{Height: f.height + 1, Lane: lane.String()}, nil
}

func call(t *testing.T, s *Server, token, method string, params any) (map[string]any, *rpcError) {
	t.Helper()
	body := map[string]any{"method": method, "id": 1}
	if params != nil {
		body["params"] = params
	}
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)

	var resp struct {
		Result map[string]any `json:"result"`
		Error  *rpcError      `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("the answer cannot be read (%d): %s", w.Code, w.Body.String())
	}
	return resp.Result, resp.Error
}

func TestGetInfo(t *testing.T) {
	s := NewServer(&fakeBackend{height: 100}, "")
	res, rerr := call(t, s, "", "getinfo", nil)
	if rerr != nil {
		t.Fatalf("getinfo failed: %v", rerr.Message)
	}
	if res["network"] != "regtest" {
		t.Fatalf("expected regtest, got %v", res["network"])
	}
	if res["height"].(float64) != 100 {
		t.Fatalf("the height does not match: %v", res["height"])
	}
	if res["utxo_count"].(float64) != 7 {
		t.Fatalf("utxo_count does not match: %v", res["utxo_count"])
	}
	// The node has to report which epoch it is in.
	if _, ok := res["epoch"]; !ok {
		t.Fatal("getinfo has to report epoch")
	}
}

// getforks gives the height of each rule and the signalling split by lane.
func TestGetForks(t *testing.T) {
	st := chain.ForkStatus{TipHeight: 300, Window: 300, Forks: []chain.ForkState{
		{Fork: 2, Known: true, Scheduled: true, Height: 500},
		{Fork: 9},
	}}
	st.Blocks[mh.LaneC], st.Blocks[mh.LaneG] = 140, 160
	st.Forks[0].Signalling[mh.LaneC], st.Forks[0].Signalling[mh.LaneG] = 120, 30
	st.Forks[1].Signalling[mh.LaneG] = 4

	s := NewServer(&fakeBackend{height: 300, forks: st}, "")
	res, rerr := call(t, s, "", "getforks", nil)
	if rerr != nil {
		t.Fatalf("getforks failed: %v", rerr.Message)
	}
	if res["window"].(float64) != 300 {
		t.Fatalf("window %v, expected 300", res["window"])
	}
	blocks := res["window_blocks"].(map[string]any)
	if blocks["C"].(float64) != 140 || blocks["G"].(float64) != 160 {
		t.Fatalf("blocks per lane %v", blocks)
	}
	forks := res["forks"].([]any)
	if len(forks) != 2 {
		t.Fatalf("%d forks, expected 2", len(forks))
	}
	known, foreign := forks[0].(map[string]any), forks[1].(map[string]any)
	if known["bit"].(float64) != 2 || known["known"] != true || known["active"] != false ||
		known["height"].(float64) != 500 {
		t.Fatalf("the scheduled fork: %v", known)
	}
	sig := known["signalling"].(map[string]any)
	if sig["C"].(float64) != 120 || sig["G"].(float64) != 30 {
		t.Fatalf("signalling %v, expected 120 C and 30 G", sig)
	}
	// A bit nobody gave a height has no height in the answer — zero would read as "from genesis".
	if _, has := foreign["height"]; has || foreign["known"] != false {
		t.Fatalf("the foreign bit: %v", foreign)
	}
}

// This is the call an exchange looks at before crediting a deposit.
func TestGetFinality(t *testing.T) {
	b := &fakeBackend{height: 100}
	s := NewServer(b, "")

	res, _ := call(t, s, "", "getfinality", nil)
	if res["finalized"].(bool) {
		t.Fatal("without a quorum nothing may be reported as finalized")
	}
	b.finalized = true
	res, _ = call(t, s, "", "getfinality", nil)
	if !res["finalized"].(bool) || res["height"].(float64) != 80 {
		t.Fatalf("after finalization expected height 80, got %v", res)
	}
}

func TestSendRawTransaction(t *testing.T) {
	b := &fakeBackend{height: 10}
	s := NewServer(b, "")

	tx := &chain.Tx{Version: 1,
		Inputs:  []chain.TxIn{{PrevTxID: [32]byte{3}, PrevIndex: 0}},
		Outputs: []chain.TxOut{{Value: 50_000, Script: make([]byte, 32)}}}
	raw := hex.EncodeToString(tx.Serialize())

	body := map[string]any{"method": "sendrawtransaction", "params": map[string]string{"hex": raw}, "id": 1}
	data, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(data))
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)

	var resp struct {
		Result string    `json:"result"`
		Error  *rpcError `json:"error"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Error != nil {
		t.Fatalf("sending failed: %v", resp.Error.Message)
	}
	want := tx.TxID()
	if resp.Result != hex.EncodeToString(want[:]) {
		t.Fatalf("the returned txid does not match: %s", resp.Result)
	}
	if len(b.submitted) != 1 {
		t.Fatal("the transaction did not reach the node")
	}

	// An error from the mempool has to reach the caller.
	b.submitErr = errors.New("the fee is below the minimum")
	_, rerr := call(t, s, "", "sendrawtransaction", map[string]string{"hex": raw})
	if rerr == nil || rerr.Message != "the fee is below the minimum" {
		t.Fatalf("the error was not passed through: %v", rerr)
	}

	// Extra bytes after the transaction are refused.
	_, rerr = call(t, s, "", "sendrawtransaction", map[string]string{"hex": raw + "00"})
	if rerr == nil {
		t.Fatal("extra bytes should have been refused")
	}
}

// RPC is the node's most sensitive interface — it must not be reachable without a token.
func TestTokenRequired(t *testing.T) {
	s := NewServer(&fakeBackend{height: 1}, "secret")

	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader([]byte(`{"method":"getinfo"}`)))
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("without a token expected 401, got %d", w.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "/", bytes.NewReader([]byte(`{"method":"getinfo"}`)))
	req.Header.Set("Authorization", "Bearer wrong")
	w = httptest.NewRecorder()
	s.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("with a wrong token expected 401, got %d", w.Code)
	}

	if _, rerr := call(t, s, "secret", "getinfo", nil); rerr != nil {
		t.Fatalf("with the right token it should pass: %v", rerr.Message)
	}
}

func TestRequestLimitsAndMethods(t *testing.T) {
	s := NewServer(&fakeBackend{height: 1}, "")

	// GET is not served.
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET should give 405, it gave %d", w.Code)
	}

	// A body that is too large.
	big := bytes.Repeat([]byte("a"), MaxRequestBytes+10)
	w = httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(big)))
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("an oversized body should give 413, it gave %d", w.Code)
	}

	// An unknown method.
	if _, rerr := call(t, s, "", "madeup", nil); rerr == nil {
		t.Fatal("an unknown method has to return an error")
	}
}

func TestTemplate(t *testing.T) {
	s := NewServer(&fakeBackend{height: 5}, "")
	res, rerr := call(t, s, "", "gettemplate", map[string]string{"lane": "C"})
	if rerr != nil {
		t.Fatalf("gettemplate failed: %v", rerr.Message)
	}
	if res["lane"] != "C" || res["height"].(float64) != 6 {
		t.Fatalf("the template does not match: %v", res)
	}
	if _, rerr := call(t, s, "", "gettemplate", map[string]string{"lane": "X"}); rerr == nil {
		t.Fatal("an unknown lane has to be refused")
	}
}

// getrawtransaction has to enrich the inputs with the address and the value of the previous
// output.
//
// It is our equivalent of what Bitcoin added as `getblock` verbosity 3. Without it the explorer
// has to fetch another transaction for every input — one block with 200 transactions suddenly
// becomes 600 calls instead of one.
func TestGetRawTransactionResolvesPrevouts(t *testing.T) {
	var payKey [32]byte
	payKey[0] = 0xAA
	var changeKey [32]byte
	changeKey[0] = 0xBB

	// A coinbase that creates an output worth 10 POWM.
	cb := &chain.Tx{
		Version: 1,
		Inputs:  []chain.TxIn{{PrevIndex: chain.CoinbaseIndex}},
		Outputs: []chain.TxOut{{Value: 1_000_000_000, Script: payKey[:]}},
	}
	cbID := cb.TxID()

	// A spend: it spends that output, sends 6 POWM and returns 3.9 as change (0.1 is the fee).
	spend := &chain.Tx{
		Version: 1,
		Inputs:  []chain.TxIn{{PrevTxID: cbID, PrevIndex: 0}},
		Outputs: []chain.TxOut{
			{Value: 600_000_000, Script: changeKey[:]},
			{Value: 390_000_000, Script: payKey[:]},
		},
	}
	spendID := spend.TxID()

	mk := func(height uint32, txs ...*chain.Tx) *chain.Block {
		b := &chain.Block{
			Header: &chain.Header{Version: 1, Height: height, Time: 1_700_000_000, Lane: mh.LaneC},
			Txs:    txs,
		}
		b.Header.MerkleRoot = chain.TxMerkleRoot(b.Txs)
		return b
	}
	cbBlk, spendBlk := mk(100, cb), mk(101, spend)

	be := &fakeBackend{height: 110, txs: map[[32]byte]txEntry{
		cbID:    {tx: cb, blk: cbBlk, idx: 0},
		spendID: {tx: spend, blk: spendBlk, idx: 0},
	}}
	srv := NewServer(be, "secret")

	var res struct {
		Result struct {
			Confirmations uint64 `json:"confirmations"`
			Coinbase      bool   `json:"coinbase"`
			ValueIn       int64  `json:"value_in_cogs"`
			ValueOut      int64  `json:"value_out_cogs"`
			Fee           int64  `json:"fee_cogs"`
			Vin           []struct {
				TxID    string `json:"txid"`
				Vout    uint32 `json:"vout"`
				Value   int64  `json:"value_cogs"`
				Address string `json:"address"`
			} `json:"vin"`
			Vout []struct {
				Value   int64  `json:"value_cogs"`
				Address string `json:"address"`
			} `json:"vout"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	callJSON(t, srv, "getrawtransaction",
		map[string]any{"txid": hex.EncodeToString(spendID[:])}, &res)
	if res.Error != nil {
		t.Fatalf("error: %s", res.Error.Message)
	}
	r := res.Result

	if r.Coinbase {
		t.Fatal("a spend is not a coinbase")
	}
	if r.Confirmations != 10 { // tip 110, block 101
		t.Fatalf("confirmations %d, expected 10", r.Confirmations)
	}
	if len(r.Vin) != 1 {
		t.Fatalf("expected one input, got %d", len(r.Vin))
	}
	if r.Vin[0].Value != 1_000_000_000 {
		t.Fatalf("the input has value %d — the prevout was not resolved", r.Vin[0].Value)
	}
	wantAddr, err := address.Encode(chain.RegTest.Bech32HRP, 0, payKey[:])
	if err != nil {
		t.Fatal(err)
	}
	if r.Vin[0].Address != wantAddr {
		t.Fatalf("the input address is %q, expected %q", r.Vin[0].Address, wantAddr)
	}
	if r.ValueIn != 1_000_000_000 || r.ValueOut != 990_000_000 {
		t.Fatalf("amounts: in %d, out %d", r.ValueIn, r.ValueOut)
	}
	if r.Fee != 10_000_000 {
		t.Fatalf("the fee is %d, expected 10000000", r.Fee)
	}
	for i, o := range r.Vout {
		if o.Address == "" {
			t.Fatalf("output %d has no address", i)
		}
	}
}

// A coinbase has nothing to resolve and must not report a fee.
func TestGetRawTransactionCoinbaseHasNoFee(t *testing.T) {
	cb := &chain.Tx{
		Version: 1,
		Inputs:  []chain.TxIn{{PrevIndex: chain.CoinbaseIndex}},
		Outputs: []chain.TxOut{{Value: 4_000_000_000, Script: make([]byte, 32)}},
	}
	id := cb.TxID()
	blk := &chain.Block{
		Header: &chain.Header{Version: 1, Height: 50, Time: 1_700_000_000, Lane: mh.LaneG},
		Txs:    []*chain.Tx{cb},
	}
	blk.Header.MerkleRoot = chain.TxMerkleRoot(blk.Txs)

	be := &fakeBackend{height: 60, txs: map[[32]byte]txEntry{id: {tx: cb, blk: blk, idx: 0}}}
	srv := NewServer(be, "secret")

	var res struct {
		Result map[string]any `json:"result"`
	}
	callJSON(t, srv, "getrawtransaction", map[string]any{"txid": hex.EncodeToString(id[:])}, &res)
	if res.Result["coinbase"] != true {
		t.Fatal("it has to be marked as a coinbase")
	}
	if _, ok := res.Result["fee_cogs"]; ok {
		t.Fatal("a coinbase has no fee — the field must not be there at all")
	}
	if _, ok := res.Result["value_in_cogs"]; ok {
		t.Fatal("a coinbase has no input value")
	}
}

// Without the index the answer has to be distinguishable from "the transaction does not exist".
func TestGetRawTransactionWithoutIndex(t *testing.T) {
	srv := NewServer(&fakeBackend{height: 10}, "secret")
	var res struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	callJSON(t, srv, "getrawtransaction", map[string]any{"txid": hex.EncodeToString(make([]byte, 32))}, &res)
	if res.Error == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(res.Error.Message, "index") {
		t.Fatalf("the error has to say that the index is missing: %q", res.Error.Message)
	}
}

// callJSON is `call` that parses the answer into a typed structure — with transactions
// `map[string]any` is awkward and `float64` on the amounts would hide exactly what we are testing.
func callJSON(t *testing.T, s *Server, method string, params any, out any) {
	t.Helper()
	body := map[string]any{"method": method, "id": 1}
	if params != nil {
		body["params"] = params
	}
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw))
	req.Header.Set("Authorization", "Bearer secret")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)
	if err := json.Unmarshal(w.Body.Bytes(), out); err != nil {
		t.Fatalf("the answer cannot be read (%d): %s", w.Code, w.Body.String())
	}
}

// The direction of a movement has to be computed from the **net amount per transaction**, not
// from whether the address is among the outputs. Change comes back to the same address, so a
// naive "in" filter does not filter at all and "out" duplicates the row for every spent output.
func TestGetAddressComputesNetDirection(t *testing.T) {
	var txPay, txSelf, txRecv [32]byte
	txPay[0], txSelf[0], txRecv[0] = 1, 2, 3

	be := &fakeBackend{height: 200, addr: map[string]addrEntry{
		"rpowm1qtest": {
			sum: store.AddrSummary{Received: 1_690_000_000, Sent: 1_000_000_000, TxCount: 3},
			hist: []store.AddrEntry{
				// A payment out: it spent 1 000 M, 690 M came back as change.
				{Height: 120, TxID: txPay, Received: false, Value: 1_000_000_000},
				{Height: 120, TxID: txPay, Received: true, Value: 690_000_000},
				// A transfer to itself: as much left as arrived.
				{Height: 110, TxID: txSelf, Received: false, Value: 500_000_000},
				{Height: 110, TxID: txSelf, Received: true, Value: 500_000_000},
				// A plain receive.
				{Height: 100, TxID: txRecv, Received: true, Value: 1_000_000_000},
			},
		},
	}}
	srv := NewServer(be, "secret")

	var res struct {
		Result struct {
			BalanceCogs int64 `json:"balance_cogs"`
			TxCount     int   `json:"tx_count"`
			Txs         []struct {
				TxID      string `json:"txid"`
				NetCogs   int64  `json:"net_cogs"`
				Direction string `json:"direction"`
			} `json:"txs"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	callJSON(t, srv, "getaddress", map[string]any{"address": "rpowm1qtest"}, &res)
	if res.Error != nil {
		t.Fatalf("error: %s", res.Error.Message)
	}
	r := res.Result

	if r.BalanceCogs != 690_000_000 {
		t.Fatalf("balance %d, expected 690000000", r.BalanceCogs)
	}
	// Five movements but **three** transactions — a row is a transaction, not a movement.
	if len(r.Txs) != 3 {
		t.Fatalf("expected 3 rows (transactions), got %d", len(r.Txs))
	}
	want := []struct {
		net int64
		dir string
	}{
		{-310_000_000, "out"}, // 690 M arrived, 1 000 M left
		{0, "self"},           // a transfer to itself
		{1_000_000_000, "in"},
	}
	for i, w := range want {
		if r.Txs[i].NetCogs != w.net || r.Txs[i].Direction != w.dir {
			t.Fatalf("row %d: net %d/%s, expected %d/%s",
				i, r.Txs[i].NetCogs, r.Txs[i].Direction, w.net, w.dir)
		}
	}
}

// Without the address index the answer has to be distinguishable.
func TestGetAddressWithoutIndex(t *testing.T) {
	srv := NewServer(&fakeBackend{height: 10}, "secret")
	var res struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	callJSON(t, srv, "getaddress", map[string]any{"address": "rpowm1qtest"}, &res)
	if res.Error == nil || !strings.Contains(res.Error.Message, "index") {
		t.Fatalf("the error has to say that the index is missing: %+v", res.Error)
	}
}

// The shares in the rich list have to add up to at most 1000 per mille. When an address appears
// in the list twice or the share is computed from a different total, it comes out higher — and
// that is exactly what iquidus has as the issue "Richlist pie chart sums to 126%".
func TestGetRichListSharesDoNotExceedSupply(t *testing.T) {
	mkKey := func(b byte) [32]byte {
		var k [32]byte
		k[0] = b
		return k
	}
	k1, k2, k3 := mkKey(1), mkKey(2), mkKey(3)
	be := &fakeBackend{
		height: 100,
		supply: 1_000_000_000,
		rich: []store.RichEntry{
			{Key: store.ScriptKey(k1), Balance: 500_000_000, Script: k1[:]},
			{Key: store.ScriptKey(k2), Balance: 300_000_000, Script: k2[:]},
			{Key: store.ScriptKey(k3), Balance: 100_000_000, Script: k3[:]},
		},
	}
	srv := NewServer(be, "secret")

	var res struct {
		Result struct {
			SupplyCogs int64 `json:"supply_cogs"`
			Addresses  []struct {
				Rank          int    `json:"rank"`
				BalanceCogs   int64  `json:"balance_cogs"`
				SharePermille int64  `json:"share_permille"`
				Address       string `json:"address"`
			} `json:"addresses"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	callJSON(t, srv, "getrichlist", map[string]any{"limit": 10}, &res)
	if res.Error != nil {
		t.Fatalf("error: %s", res.Error.Message)
	}
	rows := res.Result.Addresses
	if len(rows) != 3 {
		t.Fatalf("expected 3 rows, got %d", len(rows))
	}

	var sumShare, sumBalance int64
	for i, r := range rows {
		if r.Rank != i+1 {
			t.Fatalf("row %d has rank %d", i, r.Rank)
		}
		if r.Address == "" {
			t.Fatalf("row %d has no address", i)
		}
		sumShare += r.SharePermille
		sumBalance += r.BalanceCogs
	}
	if sumShare > 1000 {
		t.Fatalf("the shares added up to %d per mille — more than the whole supply", sumShare)
	}
	if sumBalance > res.Result.SupplyCogs {
		t.Fatalf("the balances in the list (%d) exceed the supply (%d)", sumBalance, res.Result.SupplyCogs)
	}
	// 500/1000 M = 500 per mille.
	if rows[0].SharePermille != 500 {
		t.Fatalf("the richest address has %d per mille, expected 500", rows[0].SharePermille)
	}
}

func TestGetRichListWithoutIndex(t *testing.T) {
	srv := NewServer(&fakeBackend{height: 10}, "secret")
	var res struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	callJSON(t, srv, "getrichlist", nil, &res)
	if res.Error == nil || !strings.Contains(res.Error.Message, "index") {
		t.Fatalf("the error has to say that the index is missing: %+v", res.Error)
	}
}

// Bitcoin-style tools send parameters **positionally**, not by name: btc-rpc-explorer, eiquidus
// and ElectrumX all do it exclusively this way. If we only took objects, three out of four
// clients would bounce off us.
func TestPositionalParamsWorkLikeNamed(t *testing.T) {
	cb := &chain.Tx{
		Version: 1,
		Inputs:  []chain.TxIn{{PrevIndex: chain.CoinbaseIndex}},
		Outputs: []chain.TxOut{{Value: 4_000_000_000, Script: make([]byte, 32)}},
	}
	id := cb.TxID()
	blk := &chain.Block{
		Header: &chain.Header{Version: 1, Height: 50, Time: 1_700_000_000, Lane: mh.LaneG},
		Txs:    []*chain.Tx{cb},
	}
	blk.Header.MerkleRoot = chain.TxMerkleRoot(blk.Txs)
	be := &fakeBackend{height: 60, txs: map[[32]byte]txEntry{id: {tx: cb, blk: blk, idx: 0}}}
	srv := NewServer(be, "secret")

	hexID := hex.EncodeToString(id[:])
	var named, positional struct {
		Result map[string]any `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	callJSON(t, srv, "getrawtransaction", map[string]any{"txid": hexID}, &named)
	callJSON(t, srv, "getrawtransaction", []any{hexID}, &positional)

	if named.Error != nil {
		t.Fatalf("the named shape failed: %s", named.Error.Message)
	}
	if positional.Error != nil {
		t.Fatalf("the positional shape failed: %s", positional.Error.Message)
	}
	if positional.Result["txid"] != named.Result["txid"] {
		t.Fatalf("the positional shape gave a different result: %v vs %v",
			positional.Result["txid"], named.Result["txid"])
	}
}

// An empty `params: []` is sent by btc-rpc-explorer even where there are no parameters — that
// must not be an error.
func TestEmptyPositionalParamsAreAccepted(t *testing.T) {
	srv := NewServer(&fakeBackend{height: 100}, "")
	var res struct {
		Result map[string]any `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	callJSON(t, srv, "getinfo", []any{}, &res)
	if res.Error != nil {
		t.Fatalf("an empty parameter array must not be an error: %s", res.Error.Message)
	}
	if res.Result["network"] != "regtest" {
		t.Fatalf("getinfo returned %v", res.Result["network"])
	}
}

// Too many positional parameters has to be an error with an explanation, not silent ignoring —
// silent ignoring would mean the client gets an answer to a different question than it asked.
func TestTooManyPositionalParamsIsAnError(t *testing.T) {
	srv := NewServer(&fakeBackend{height: 10}, "secret")
	var res struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	callJSON(t, srv, "getrichlist", []any{10, "extra", "and more"}, &res)
	if res.Error == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(res.Error.Message, "at most") {
		t.Fatalf("the error has to say how many parameters the method takes: %q", res.Error.Message)
	}
}

// A spent output is `null`, not an error — that is exactly how `gettxout` is used to check
// whether an output is still available.
func TestGetTxOutSpentIsNull(t *testing.T) {
	var txid [32]byte
	txid[0] = 7
	script := make([]byte, 32)
	script[0] = 0xAB

	be := &fakeBackend{height: 100, coins: map[chain.Outpoint]chain.Coin{
		{TxID: txid, Index: 0}: {Value: 12345, Script: script, Height: 90},
	}}
	srv := NewServer(be, "secret")
	hexID := hex.EncodeToString(txid[:])

	var live struct {
		Result *struct {
			ValueCogs     int64  `json:"value_cogs"`
			Confirmations uint64 `json:"confirmations"`
			Address       string `json:"address"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	callJSON(t, srv, "gettxout", map[string]any{"txid": hexID, "vout": 0}, &live)
	if live.Error != nil {
		t.Fatalf("error: %s", live.Error.Message)
	}
	if live.Result == nil {
		t.Fatal("an existing output was not found")
	}
	if live.Result.ValueCogs != 12345 {
		t.Fatalf("value %d", live.Result.ValueCogs)
	}
	if live.Result.Confirmations != 11 { // tip 100, created at 90
		t.Fatalf("confirmations %d, expected 11", live.Result.Confirmations)
	}
	if live.Result.Address == "" {
		t.Fatal("the output has no address")
	}

	var spent struct {
		Result *map[string]any `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	callJSON(t, srv, "gettxout", map[string]any{"txid": hexID, "vout": 1}, &spent)
	if spent.Error != nil {
		t.Fatalf("a spent output must not be an error: %s", spent.Error.Message)
	}
	if spent.Result != nil {
		t.Fatalf("a spent output has to be null, it is %v", *spent.Result)
	}
}

func TestBlockHashAndCountRoundTrip(t *testing.T) {
	be := &fakeBackend{height: 42}
	srv := NewServer(be, "secret")

	var cnt struct {
		Result uint64 `json:"result"`
	}
	callJSON(t, srv, "getblockcount", nil, &cnt)
	if cnt.Result != 42 {
		t.Fatalf("getblockcount returned %d", cnt.Result)
	}

	var best struct {
		Result string `json:"result"`
	}
	callJSON(t, srv, "getbestblockhash", nil, &best)
	if len(best.Result) != 64 {
		t.Fatalf("getbestblockhash returned %q", best.Result)
	}

	// fakeBackend has no height index, so getblockhash has to say we do not have the block —
	// not return an empty hash.
	var bh struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	callJSON(t, srv, "getblockhash", []any{7}, &bh)
	if bh.Error == nil {
		t.Fatal("a missing block has to be an error, not an empty hash")
	}
}

// getinfo has to carry what a **real** progress indicator can be built from: how many peers the
// node has, how far the chain reaches according to them and the height the UTXO set is finished
// up to. Without a target the wallet could only draw an endless animation.
func TestGetinfoCarriesSyncState(t *testing.T) {
	b := &fakeBackend{height: 400, syncPeers: 3, syncBest: 1000, syncApplied: 250}
	var res struct {
		Result map[string]any `json:"result"`
	}
	callJSON(t, NewServer(b, "secret"), "getinfo", nil, &res)
	out := res.Result

	for k, want := range map[string]float64{
		"peers": 3, "best_known": 1000, "applied_height": 250, "height": 400,
	} {
		if got, _ := out[k].(float64); got != want {
			t.Errorf("%s = %v, expected %v", k, out[k], want)
		}
	}
	if synced, _ := out["synced"].(bool); synced {
		t.Error("a node with the UTXO set at 250 and peers at 1000 is not synced")
	}

	// A caught-up node: the UTXO set at the tip and the tip at least where the peers are.
	b2 := &fakeBackend{height: 1000, syncPeers: 3, syncBest: 1000, syncApplied: 1000}
	var res2 struct {
		Result map[string]any `json:"result"`
	}
	callJSON(t, NewServer(b2, "secret"), "getinfo", nil, &res2)
	if synced, _ := res2.Result["synced"].(bool); !synced {
		t.Error("a caught-up node reports itself as not synced")
	}
}
