package walletrpc

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"powmachina/internal/chain"
	"powmachina/internal/wallet"
	"powmachina/internal/wallet/wallettest"
)

func init() { wallet.DefaultKDF = wallet.KDFParams{MemoryKiB: 8 << 10, Time: 1, Threads: 1} }

const token = "secret"

func single(w *wallet.Wallet, n wallet.Node) *Server {
	s := New(n, nil, token)
	if err := s.AddWallet("exchange", w); err != nil {
		panic(err)
	}
	return s
}

type env struct {
	t    *testing.T
	w    *wallet.Wallet
	node *wallettest.Node
	srv  *httptest.Server
}

// setup: a wallet with two matured rewards of 40 POWM each behind wallet-rpc.
func setup(t *testing.T) *env {
	t.Helper()
	w, _, err := wallet.Create(filepath.Join(t.TempDir(), "exchange"), "password", chain.RegTest, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { w.Close() })
	n := wallettest.NewNode()
	addr, _ := w.NewAddress("rewards")
	pk, _ := w.DecodeAddress(addr)
	n.Mine(&pk, nil)
	n.Mine(&pk, nil)
	for range chain.CoinbaseMaturity {
		n.Mine(nil, nil)
	}
	if _, err := w.Sync(n); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(single(w, n))
	t.Cleanup(srv.Close)
	return &env{t: t, w: w, node: n, srv: srv}
}

type reply struct {
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

func (e *env) raw(body string, auth string) (*http.Response, reply) {
	e.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, e.srv.URL, strings.NewReader(body))
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	var r reply
	_ = json.NewDecoder(resp.Body).Decode(&r)
	return resp, r
}

// call: the bitcoin style — JSON-RPC 1.0, positional parameters, Basic auth.
func (e *env) call(method string, params ...any) reply {
	e.t.Helper()
	if params == nil {
		params = []any{}
	}
	body, _ := json.Marshal(map[string]any{"jsonrpc": "1.0", "id": "t", "method": method, "params": params})
	basic := "Basic " + base64.StdEncoding.EncodeToString([]byte("rpcuser:"+token))
	_, r := e.raw(string(body), basic)
	return r
}

func (e *env) ok(method string, params ...any) json.RawMessage {
	e.t.Helper()
	r := e.call(method, params...)
	if r.Error != nil {
		e.t.Fatalf("%s: %d %s", method, r.Error.Code, r.Error.Message)
	}
	return r.Result
}

func (e *env) mine() {
	e.node.Mine(nil, nil)
	if _, err := e.w.Sync(e.node); err != nil {
		e.t.Fatal(err)
	}
}

func TestAuth(t *testing.T) {
	e := setup(t)
	body := `{"method":"getbalance","params":[],"id":1}`
	if resp, _ := e.raw(body, ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("without a token: %d", resp.StatusCode)
	}
	if resp, _ := e.raw(body, "Bearer wrong"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a wrong token: %d", resp.StatusCode)
	}
	if resp, _ := e.raw(body, "Basic "+base64.StdEncoding.EncodeToString([]byte("x:wrong"))); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a wrong Basic password: %d", resp.StatusCode)
	}
	if resp, r := e.raw(body, "Bearer "+token); resp.StatusCode != 200 || r.Error != nil {
		t.Fatalf("Bearer: %d %+v", resp.StatusCode, r.Error)
	}
}

// Amounts go as numbers with eight decimal places and are read exactly.
func TestBalanceAndExactAmounts(t *testing.T) {
	e := setup(t)
	if got := string(e.ok("getbalance")); got != "80.00000000" {
		t.Fatalf("getbalance = %s", got)
	}
	other, _, _ := wallet.Create(filepath.Join(t.TempDir(), "other"), "x", chain.RegTest, 0)
	defer other.Close()
	to, _ := other.NewAddress("")

	// 0.1 both as a number and as a string — both exactly 10 000 000 cogs.
	e.ok("sendtoaddress", to, json.Number("0.1"))
	e.ok("sendtoaddress", to, "0.2")
	e.mine()
	other.Sync(e.node)
	if b := other.Balance(1).Spendable; b != 30_000_000 {
		t.Fatalf("the recipient has %d cogs, expected exactly 30 000 000", b)
	}
	for _, bad := range []any{json.Number("1e-8"), json.Number("0.000000001"), "-1", json.Number("0")} {
		if r := e.call("sendtoaddress", to, bad); r.Error == nil || r.Error.Code != codeInvalidParam {
			t.Fatalf("amount %v: %+v", bad, r.Error)
		}
	}
}

func TestErrorCodes(t *testing.T) {
	e := setup(t)
	if r := e.call("sendtoaddress", "powm1something", 1); r.Error == nil || r.Error.Code != codeInvalidAddress {
		t.Fatalf("a bad address: %+v", r.Error)
	}
	to, _ := e.w.NewAddress("")
	if r := e.call("sendtoaddress", to, 1000); r.Error == nil || r.Error.Code != codeInsufficient {
		t.Fatalf("insufficient funds: %+v", r.Error)
	}
	if r := e.call("nosuchmethod"); r.Error == nil || r.Error.Code != codeMethodNotFound {
		t.Fatalf("an unknown method: %+v", r.Error)
	}
}

// An exchange's flow: an address for a user, a deposit, listsinceblock, a withdrawal through sendmany.
func TestExchangeFlow(t *testing.T) {
	e := setup(t)
	var deposit string
	json.Unmarshal(e.ok("getnewaddress", "user-42"), &deposit)
	var v struct {
		IsValid bool `json:"isvalid"`
		IsMine  bool `json:"ismine"`
	}
	json.Unmarshal(e.ok("validateaddress", deposit), &v)
	if !v.IsValid || !v.IsMine {
		t.Fatalf("validateaddress of our own address: %+v", v)
	}

	// Somebody outside sends a deposit of 12.5 POWM.
	payer, _, _ := wallet.Create(filepath.Join(t.TempDir(), "payer"), "x", chain.RegTest, 0)
	defer payer.Close()
	pa, _ := payer.NewAddress("")
	ppk, _ := payer.DecodeAddress(pa)
	e.node.Mine(&ppk, nil)
	for range chain.CoinbaseMaturity {
		e.node.Mine(nil, nil)
	}
	payer.Sync(e.node)
	var sinceHash string
	{
		var r struct {
			LastBlock string `json:"lastblock"`
		}
		e.w.Sync(e.node)
		json.Unmarshal(e.ok("listsinceblock"), &r)
		sinceHash = r.LastBlock
	}
	dpk, _ := e.w.DecodeAddress(deposit)
	if _, _, err := payer.Send(e.node, []wallet.Payment{{To: dpk, Amount: 1_250_000_000}}, 0); err != nil {
		t.Fatal(err)
	}
	e.mine()

	var since struct {
		Transactions []map[string]any `json:"transactions"`
		LastBlock    string           `json:"lastblock"`
	}
	json.Unmarshal(e.ok("listsinceblock", sinceHash), &since)
	var found bool
	for _, tx := range since.Transactions {
		if tx["address"] == deposit && tx["category"] == "receive" {
			if tx["amount"].(float64) != 12.5 || tx["label"] != "user-42" {
				t.Fatalf("deposit: %+v", tx)
			}
			found = true
		}
	}
	if !found {
		t.Fatalf("listsinceblock did not find the deposit: %+v", since.Transactions)
	}
	if string(e.ok("getreceivedbyaddress", deposit)) != "12.50000000" {
		t.Fatal("getreceivedbyaddress does not match")
	}

	// A withdrawal to two users at once.
	o1, _, _ := wallet.Create(filepath.Join(t.TempDir(), "o1"), "x", chain.RegTest, 0)
	defer o1.Close()
	a1, _ := o1.NewAddress("")
	a2, _ := o1.NewAddress("")
	var txid string
	json.Unmarshal(e.ok("sendmany", "", map[string]any{a1: json.Number("1.5"), a2: "2.25"}), &txid)
	var gt struct {
		Amount  json.Number      `json:"amount"`
		Fee     json.Number      `json:"fee"`
		Details []map[string]any `json:"details"`
	}
	json.Unmarshal(e.ok("gettransaction", txid), &gt)
	if gt.Amount != "-3.75000000" || !strings.HasPrefix(string(gt.Fee), "-0.") || len(gt.Details) != 2 {
		t.Fatalf("gettransaction sendmany: %+v", gt)
	}
	e.mine()
	o1.Sync(e.node)
	if o1.Balance(1).Spendable != 375_000_000 {
		t.Fatalf("the sendmany recipients have %d", o1.Balance(1).Spendable)
	}

	// listunspent sees both the deposit and the change, listtransactions has both send and receive.
	var utxos []map[string]any
	json.Unmarshal(e.ok("listunspent", 1, 9999999, []string{deposit}), &utxos)
	if len(utxos) != 1 || utxos[0]["amount"].(float64) != 12.5 {
		t.Fatalf("listunspent with a filter: %+v", utxos)
	}
	var hist []map[string]any
	json.Unmarshal(e.ok("listtransactions", "*", 100), &hist)
	cats := map[any]int{}
	for _, h := range hist {
		cats[h["category"]]++
	}
	if cats["send"] != 2 || cats["receive"] != 1 || cats["generate"] != 2 {
		t.Fatalf("the history categories: %v", cats)
	}
}

func TestDumpImportAndNamedParams(t *testing.T) {
	e := setup(t)
	addr := e.w.Addresses()[0].Address
	var secret string
	json.Unmarshal(e.ok("dumpprivkey", addr), &secret)
	if !strings.HasPrefix(secret, "rpowmsec1") {
		t.Fatalf("dumpprivkey: %s", secret)
	}

	w2, _, _ := wallet.Create(filepath.Join(t.TempDir(), "second"), "x", chain.RegTest, 0)
	defer w2.Close()
	w2.Sync(e.node)
	srv2 := httptest.NewServer(single(w2, e.node))
	defer srv2.Close()
	// named parameters, JSON-RPC 2.0, Bearer
	body := `{"jsonrpc":"2.0","id":1,"method":"importprivkey","params":{"privkey":"` + secret + `","label":"old"}}`
	req, _ := http.NewRequest(http.MethodPost, srv2.URL, bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var r reply
	json.NewDecoder(resp.Body).Decode(&r)
	resp.Body.Close()
	if r.Error != nil {
		t.Fatalf("importprivkey: %+v", r.Error)
	}
	if w2.Balance(1).Spendable != 80*chain.POWM {
		t.Fatalf("after the import with a rescan: %+v", w2.Balance(1))
	}
}

func TestBatch(t *testing.T) {
	e := setup(t)
	body := `[{"method":"getbalance","params":[],"id":1},{"method":"getwalletinfo","params":[],"id":2}]`
	req, _ := http.NewRequest(http.MethodPost, e.srv.URL, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out []reply
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || len(out) != 2 || out[0].Error != nil {
		t.Fatalf("batch: %v %+v", err, out)
	}
}
