package walletrpc

import (
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

// An exchange: a deposit wallet (two matured rewards = two "deposits") and a hot wallet, both in one
// wallet-rpc, addressed through /wallet/<name> as in bitcoind.
type exchange struct {
	t        *testing.T
	dep, hot *wallet.Wallet
	node     *wallettest.Node
	srv      *httptest.Server
	s        *Server
}

func newExchange(t *testing.T, requirePass bool) *exchange {
	t.Helper()
	dir := t.TempDir()
	dep, _, err := wallet.Create(filepath.Join(dir, "deposit"), "pass-dep", chain.RegTest, 0)
	if err != nil {
		t.Fatal(err)
	}
	hot, _, err := wallet.Create(filepath.Join(dir, "hot"), "pass-hot", chain.RegTest, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dep.Close(); hot.Close() })
	n := wallettest.NewNode()
	for range 2 {
		a, _ := dep.NewAddress("user")
		pk, _ := dep.DecodeAddress(a)
		n.Mine(&pk, nil)
	}
	for range chain.CoinbaseMaturity {
		n.Mine(nil, nil)
	}
	dep.Sync(n)
	hot.Sync(n)
	s := New(n, nil, token)
	s.RequirePassphrase = requirePass
	s.AddWallet("deposit", dep)
	s.AddWallet("hot", hot)
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	return &exchange{t: t, dep: dep, hot: hot, node: n, srv: srv, s: s}
}

func (b *exchange) call(path, method string, params ...any) reply {
	b.t.Helper()
	if params == nil {
		params = []any{}
	}
	body, _ := json.Marshal(map[string]any{"jsonrpc": "1.0", "id": 1, "method": method, "params": params})
	req, _ := http.NewRequest(http.MethodPost, b.srv.URL+path, strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("u:"+token)))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		b.t.Fatal(err)
	}
	defer resp.Body.Close()
	var r reply
	json.NewDecoder(resp.Body).Decode(&r)
	return r
}

func (b *exchange) ok(path, method string, params ...any) json.RawMessage {
	b.t.Helper()
	r := b.call(path, method, params...)
	if r.Error != nil {
		b.t.Fatalf("%s %s: %d %s", path, method, r.Error.Code, r.Error.Message)
	}
	return r.Result
}

func TestMultiwalletRouting(t *testing.T) {
	b := newExchange(t, false)
	var names []string
	json.Unmarshal(b.ok("/", "listwallets"), &names)
	if len(names) != 2 {
		t.Fatalf("listwallets: %v", names)
	}
	if r := b.call("/", "getbalance"); r.Error == nil || r.Error.Code != codeWalletNotSpec {
		t.Fatalf("getbalance on / with two wallets: %+v", r.Error)
	}
	if r := b.call("/wallet/unknown", "getbalance"); r.Error == nil || r.Error.Code != codeWalletNotFound {
		t.Fatalf("an unknown wallet: %+v", r.Error)
	}
	if got := string(b.ok("/wallet/deposit", "getbalance")); got != "80.00000000" {
		t.Fatalf("deposit getbalance %s", got)
	}
	if got := string(b.ok("/wallet/hot", "getbalance")); got != "0.00000000" {
		t.Fatalf("hot getbalance %s", got)
	}
	// ismine is checked on the wallet's endpoint.
	var addr string
	json.Unmarshal(b.ok("/wallet/hot", "getnewaddress"), &addr)
	var v struct {
		IsMine bool `json:"ismine"`
	}
	json.Unmarshal(b.ok("/wallet/deposit", "validateaddress", addr), &v)
	if v.IsMine {
		t.Fatal("the hot wallet's address is ismine in the deposit wallet")
	}
	json.Unmarshal(b.ok("/wallet/hot", "validateaddress", addr), &v)
	if !v.IsMine {
		t.Fatal("the hot wallet's address is not ismine in the hot wallet")
	}
}

func TestPassphraseGatesSigning(t *testing.T) {
	b := newExchange(t, true)
	var to string
	json.Unmarshal(b.ok("/wallet/hot", "getnewaddress"), &to)
	if r := b.call("/wallet/deposit", "sendtoaddress", to, 1); r.Error == nil || r.Error.Code != codeUnlockNeeded {
		t.Fatalf("a withdrawal without walletpassphrase: %+v", r.Error)
	}
	if r := b.call("/wallet/deposit", "walletpassphrase", "wrong", 60); r.Error == nil || r.Error.Code != codeWrongPassphrase {
		t.Fatalf("a wrong password: %+v", r.Error)
	}
	// The hot wallet's password does not unlock the deposit one.
	if r := b.call("/wallet/deposit", "walletpassphrase", "pass-hot", 60); r.Error == nil {
		t.Fatal("another wallet's password unlocked the deposit one")
	}
	b.ok("/wallet/deposit", "walletpassphrase", "pass-dep", 60)
	b.ok("/wallet/deposit", "sendtoaddress", to, 1)
	b.ok("/wallet/deposit", "walletlock")
	if r := b.call("/wallet/deposit", "dumpprivkey", to); r.Error == nil || r.Error.Code != codeUnlockNeeded {
		t.Fatalf("dumpprivkey po walletlock: %+v", r.Error)
	}
	var info map[string]any
	json.Unmarshal(b.ok("/wallet/deposit", "getwalletinfo"), &info)
	if info["unlocked_until"].(float64) != 0 {
		t.Fatalf("unlocked_until after locking: %v", info["unlocked_until"])
	}
}

// Collecting deposits the way a bitcoind integration does: createrawtransaction → fundrawtransaction with the
// change to the hot wallet → signrawtransactionwithwallet → decoderawtransaction (verifying the change)
// → sendrawtransaction. Then a second collection must not offer the same coins.
func TestDepositCollectionRawFlow(t *testing.T) {
	b := newExchange(t, true)
	var hotMain string
	json.Unmarshal(b.ok("/wallet/hot", "getnewaddress", "hot main"), &hotMain)
	b.ok("/wallet/deposit", "walletpassphrase", "pass-dep", 600)

	var utxos []struct {
		TxID string `json:"txid"`
		Vout uint32 `json:"vout"`
	}
	json.Unmarshal(b.ok("/wallet/deposit", "listunspent"), &utxos)
	var raw string
	json.Unmarshal(b.ok("/", "createrawtransaction",
		[]map[string]any{{"txid": utxos[0].TxID, "vout": utxos[0].Vout}},
		map[string]any{hotMain: json.Number("5")}), &raw)
	var funded struct {
		Hex       string      `json:"hex"`
		Fee       json.Number `json:"fee"`
		ChangePos int         `json:"changepos"`
	}
	json.Unmarshal(b.ok("/wallet/deposit", "fundrawtransaction", raw, map[string]any{"changeAddress": hotMain}), &funded)
	if funded.ChangePos < 0 {
		t.Fatal("fundrawtransaction without change")
	}
	var signed struct {
		Hex      string `json:"hex"`
		Complete bool   `json:"complete"`
	}
	json.Unmarshal(b.ok("/wallet/deposit", "signrawtransactionwithwallet", funded.Hex), &signed)
	if !signed.Complete {
		t.Fatal("the signature is incomplete")
	}
	var dec struct {
		Vout []struct {
			Value json.Number `json:"value"`
			SPK   struct {
				Address string `json:"address"`
			} `json:"scriptPubKey"`
		} `json:"vout"`
	}
	json.Unmarshal(b.ok("/", "decoderawtransaction", signed.Hex), &dec)
	if len(dec.Vout) != 2 || dec.Vout[funded.ChangePos].SPK.Address != hotMain {
		t.Fatalf("the change does not go to the hot wallet: %+v", dec.Vout)
	}
	var txid string
	json.Unmarshal(b.ok("/wallet/deposit", "sendrawtransaction", signed.Hex), &txid)

	// The second collection: the first coin is reserved, fundraw has to reach for the second.
	json.Unmarshal(b.ok("/", "createrawtransaction", []any{}, map[string]any{hotMain: json.Number("5")}), &raw)
	var funded2 struct {
		Hex string `json:"hex"`
	}
	json.Unmarshal(b.ok("/wallet/deposit", "fundrawtransaction", raw, map[string]any{"changeAddress": hotMain}), &funded2)
	json.Unmarshal(b.ok("/", "decoderawtransaction", funded2.Hex), &dec)
	var dec2 struct {
		Vin []struct {
			TxID string `json:"txid"`
		} `json:"vin"`
	}
	json.Unmarshal(b.ok("/", "decoderawtransaction", funded2.Hex), &dec2)
	for _, in := range dec2.Vin {
		if in.TxID == utxos[0].TxID {
			t.Fatal("the second collection offered a coin the first one is already spending")
		}
	}

	b.node.Mine(nil, nil)
	b.hot.Sync(b.node)
	fee, _ := wallet.ParseAmount(strings.TrimPrefix(string(funded.Fee), "-"))
	if got := b.hot.Balance(1).Spendable; got != 40*chain.POWM-fee {
		t.Fatalf("the hot wallet has %s, expected the whole coin minus the fee", wallet.FormatAmount(got))
	}
}

func TestSweepAllAndChangeAddress(t *testing.T) {
	b := newExchange(t, false)
	var hotMain string
	json.Unmarshal(b.ok("/wallet/hot", "getnewaddress"), &hotMain)
	var res struct {
		TxIDs  []string    `json:"txids"`
		Amount json.Number `json:"amount"`
	}
	json.Unmarshal(b.ok("/wallet/deposit", "sweepall", hotMain), &res)
	if len(res.TxIDs) != 1 || !strings.HasPrefix(string(res.Amount), "79.9") {
		t.Fatalf("sweepall: %+v", res)
	}
	b.node.Mine(nil, nil)
	b.hot.Sync(b.node)
	b.dep.Sync(b.node)
	if string(b.ok("/wallet/deposit", "getbalance")) != "0.00000000" {
		t.Fatal("something was left in the deposit wallet after sweepall")
	}

	// The hot wallet's fixed change address: getrawchangeaddress returns it and a withdrawal uses it.
	if err := b.hot.SetChangeAddress(hotMain); err != nil {
		t.Fatal(err)
	}
	var ch string
	json.Unmarshal(b.ok("/wallet/hot", "getrawchangeaddress"), &ch)
	if ch != hotMain {
		t.Fatalf("getrawchangeaddress %s", ch)
	}
}

// sweepall with estimate says what would happen and does nothing — and what it says is what then happens.
func TestSweepAllEstimate(t *testing.T) {
	b := newExchange(t, true)
	var to string
	json.Unmarshal(b.ok("/wallet/hot", "getnewaddress"), &to)
	var est struct {
		Estimate     bool
		Amount, Fee  json.Number
		Transactions int
		Inputs       int
	}
	// No walletpassphrase before this: an estimate must not need the password.
	json.Unmarshal(b.ok("/wallet/deposit", "sweepall", to, 1, 500, true), &est)
	if !est.Estimate || est.Transactions != 1 || est.Inputs != 2 || !strings.HasPrefix(string(est.Amount), "79.9") {
		t.Fatalf("estimate: %+v", est)
	}
	if string(b.ok("/wallet/deposit", "getbalance")) != "80.00000000" {
		t.Fatal("an estimate moved coins")
	}
	if r := b.call("/wallet/deposit", "sweepall", to); r.Error == nil {
		t.Fatal("a real sweep went through without the password")
	}
	b.ok("/wallet/deposit", "walletpassphrase", "pass-dep", 60)
	var res struct {
		TxIDs  []string `json:"txids"`
		Amount json.Number
	}
	json.Unmarshal(b.ok("/wallet/deposit", "sweepall", to), &res)
	if len(res.TxIDs) != est.Transactions || res.Amount != est.Amount {
		t.Fatalf("estimated %+v, swept %+v", est, res)
	}
}

func TestListReceivedByAddress(t *testing.T) {
	b := newExchange(t, false)
	type row struct {
		Address       string
		Amount        json.Number
		Confirmations uint64
		Label         string
		TxIDs         []string `json:"txids"`
		Imported      bool
	}
	var rows []row
	json.Unmarshal(b.ok("/wallet/deposit", "listreceivedbyaddress"), &rows)
	if len(rows) != 2 {
		t.Fatalf("%d addresses with payments, expected the two that were mined to: %+v", len(rows), rows)
	}
	for _, r := range rows {
		if r.Amount != "40.00000000" || r.Label != "user" || len(r.TxIDs) != 1 || r.Confirmations == 0 || r.Imported {
			t.Fatalf("row %+v", r)
		}
		if string(b.ok("/wallet/deposit", "getreceivedbyaddress", r.Address)) != string(r.Amount) {
			t.Fatal("listreceivedbyaddress disagrees with getreceivedbyaddress")
		}
	}
	// An address nobody paid yet shows up only with include_empty — and then with zero, not missing.
	var fresh string
	json.Unmarshal(b.ok("/wallet/deposit", "getnewaddress", "empty one"), &fresh)
	json.Unmarshal(b.ok("/wallet/deposit", "listreceivedbyaddress"), &rows)
	if len(rows) != 2 {
		t.Fatal("an unpaid address is listed without include_empty")
	}
	rows = nil
	json.Unmarshal(b.ok("/wallet/deposit", "listreceivedbyaddress", 1, true), &rows)
	if len(rows) != 3 || rows[2].Address != fresh || rows[2].Amount != "0.00000000" || rows[2].Label != "empty one" || rows[2].TxIDs == nil {
		t.Fatalf("include_empty: %+v", rows)
	}
	// The hot wallet received nothing.
	rows = nil
	json.Unmarshal(b.ok("/wallet/hot", "listreceivedbyaddress"), &rows)
	if len(rows) != 0 {
		t.Fatalf("the hot wallet lists %+v", rows)
	}
}
