// Package walletrpc is the wallet's JSON-RPC server (powmachina-wallet-rpc) in the shape bitcoin-style
// tools and exchange adapters know: getnewaddress, getbalance, listunspent, sendtoaddress, sendmany,
// gettransaction, listtransactions, listsinceblock, validateaddress, dumpprivkey, importprivkey…
//
// The differences from bitcoind worth knowing:
//   - Amounts are in POWM with eight decimal places. On the wire they are numbers (as in bitcoind),
//     but the server reads and writes them **as text** — never through a float, so 0.1 is exactly 0.1.
//     The input may be a number or a string; an exponent (1e-8) is refused.
//   - The chain's read methods (getblockcount, getblock, getrawtransaction, sendrawtransaction…) are
//     forwarded to the node. An adapter therefore has one endpoint, as with bitcoind.
//   - The wallet is unlocked from startup (the password from the environment); walletpassphrase is not needed.
package walletrpc

import (
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"powmachina/internal/chain"
	"powmachina/internal/nodeclient"
	"powmachina/internal/wallet"
)

// The bitcoin error codes — adapters make decisions by them.
const (
	codeMisc            = -1
	codeInvalidAddress  = -5
	codeInsufficient    = -6
	codeInvalidParam    = -8
	codeWalletError     = -4
	codeMethodNotFound  = -32601
	codeParse           = -32700
	codeInvalidKey      = -5
	codeNodeUnavailable = -28
	codeUnlockNeeded    = -13
	codeWrongPassphrase = -14
	codeWalletNotFound  = -18
	codeWalletNotSpec   = -19
	codeDeserialize     = -22
	codeVerifyRejected  = -26
)

// MaxRequestBytes is the cap on the request body.
const MaxRequestBytes = 1 << 20

// Server serves one or more wallets (like the bitcoind multiwallet: `/wallet/<name>`).
type Server struct {
	// mu protects `wallets` and `order`. Since RPC can change them too (createwallet/loadwallet with
	// `-allow-manage`), it is no longer enough that they were filled at startup: a write during the
	// handling of another request would be a race over the map.
	mu      sync.RWMutex
	wallets map[string]*loaded
	order   []string
	node    wallet.Node
	fwd     *nodeclient.Client // nil = no forwarding (tests)
	token   string
	// FeePerKB is the fee rate (0 = the network minimum).
	FeePerKB int64
	// RequirePassphrase: signing (withdrawals, signraw, sweep, dumpprivkey) only after walletpassphrase.
	// The RPC token alone is then not enough to spend.
	RequirePassphrase bool
	// AllowManage opens the wallet management methods (createwallet, loadwallet, getseed…).
	// For the GUI yes, for an exchange **no**: whoever obtains its RPC token must not be able to create
	// or open wallets at arbitrary paths. See manage.go.
	AllowManage bool
	// Params is the network for newly created wallets (when none is loaded yet).
	Params chain.NetParams
	// OnWalletAdded is called after a wallet is added while running (createwallet/loadwallet).
	// Without it a wallet created from the GUI would never scan blocks: the sync loop is started by main
	// at startup and it would not learn about one added later.
	OnWalletAdded func(name string, w *wallet.Wallet)
}

// loaded is a loaded wallet and its state in the server.
type loaded struct {
	w  *wallet.Wallet
	st *walletState
}

// walletState: one wallet's signing lock and send ordering.
type walletState struct {
	// sendMu: coin selection and sending go one at a time — two payments at once would pick the same
	// coin and the node would refuse the second.
	sendMu sync.Mutex
	mu     sync.Mutex
	until  time.Time
	// passHash: SHA-256 of the password after the first successful Argon2id check — further
	// walletpassphrase calls (an adapter makes one on every withdrawal) therefore do not cost a second.
	passHash  [32]byte
	passKnown bool
}

func (st *walletState) unlockedUntil() int64 {
	st.mu.Lock()
	defer st.mu.Unlock()
	if time.Now().After(st.until) {
		return 0
	}
	return st.until.Unix()
}

// walletCtx is a call over a particular wallet.
type walletCtx struct {
	*Server
	name string
	w    *wallet.Wallet
	st   *walletState
}

func (s *walletCtx) needUnlocked() error {
	if !s.RequirePassphrase || s.st.unlockedUntil() != 0 {
		return nil
	}
	return &rpcError{codeUnlockNeeded, "Error: Please enter the wallet passphrase with walletpassphrase first."}
}

// New creates the server. `token` is required for every call (Bearer or the Basic password).
func New(node wallet.Node, fwd *nodeclient.Client, token string) *Server {
	return &Server{wallets: make(map[string]*loaded), node: node, fwd: fwd, token: token}
}

// AddWallet files a wallet under a name (the path `/wallet/<name>`).
func (s *Server) AddWallet(name string, w *wallet.Wallet) error {
	s.mu.Lock()
	if _, dup := s.wallets[name]; dup {
		s.mu.Unlock()
		return fmt.Errorf("the wallet %q is loaded twice", name)
	}
	if len(s.order) > 0 && s.wallets[s.order[0]].w.Params().Magic != w.Params().Magic {
		s.mu.Unlock()
		return fmt.Errorf("the wallet %q is from another network", name)
	}
	s.wallets[name] = &loaded{w: w, st: &walletState{}}
	s.order = append(s.order, name)
	hook := s.OnWalletAdded
	s.mu.Unlock()

	// The hook (starting the sync loop) goes **outside the lock**: it calls foreign code and holding the
	// lock over the wallet map while doing so is a path to a deadlock.
	if hook != nil {
		hook(name, w)
	}
	return nil
}

// lookup returns a wallet by name. An empty name = the only loaded one, otherwise nothing.
func (s *Server) lookup(name string) (*loaded, string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if name == "" {
		if len(s.order) != 1 {
			return nil, "", false
		}
		name = s.order[0]
	}
	l, ok := s.wallets[name]
	return l, name, ok
}

// names returns a copy of the list of loaded wallets.
func (s *Server) names() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]string{}, s.order...)
}

// HTTPServer assembles the server with timeouts.
func (s *Server) HTTPServer(addr string) *http.Server {
	return &http.Server{Addr: addr, Handler: s, ReadTimeout: 15 * time.Second,
		WriteTimeout: 120 * time.Second, ReadHeaderTimeout: 5 * time.Second}
}

type request struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	ID     json.RawMessage `json:"id"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return e.Message }

// The answer as in bitcoind: both result and error are always present (one of them null).
type response struct {
	Result any             `json:"result"`
	Error  *rpcError       `json:"error"`
	ID     json.RawMessage `json:"id"`
}

func (s *Server) authorized(r *http.Request) bool {
	if s.token == "" {
		return true
	}
	h := r.Header.Get("Authorization")
	var got string
	switch {
	case strings.HasPrefix(h, "Bearer "):
		got = strings.TrimPrefix(h, "Bearer ")
	case strings.HasPrefix(h, "Basic "):
		// bitcoin clients send rpcuser:rpcpassword; the name is ignored, the password = the token
		raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(h, "Basic "))
		if err != nil {
			return false
		}
		_, pass, ok := strings.Cut(string(raw), ":")
		if !ok {
			return false
		}
		got = pass
	default:
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) == 1
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if !s.authorized(r) {
		w.Header().Set("WWW-Authenticate", `Basic realm="powmachina-wallet-rpc"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxRequestBytes+1))
	if err != nil || len(body) > MaxRequestBytes {
		http.Error(w, "request too large", http.StatusRequestEntityTooLarge)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	trimmed := strings.TrimSpace(string(body))
	if strings.HasPrefix(trimmed, "[") {
		var reqs []request
		if err := json.Unmarshal(body, &reqs); err != nil {
			_ = json.NewEncoder(w).Encode(response{Error: &rpcError{codeParse, "parse error"}})
			return
		}
		out := make([]response, 0, len(reqs))
		for _, rq := range reqs {
			out = append(out, s.handle(r.URL.Path, rq))
		}
		_ = json.NewEncoder(w).Encode(out)
		return
	}
	var rq request
	if err := json.Unmarshal(body, &rq); err != nil {
		_ = json.NewEncoder(w).Encode(response{Error: &rpcError{codeParse, "parse error"}})
		return
	}
	_ = json.NewEncoder(w).Encode(s.handle(r.URL.Path, rq))
}

func (s *Server) handle(path string, rq request) response {
	res, err := s.route(path, rq.Method, rq.Params)
	resp := response{ID: rq.ID}
	if err != nil {
		var re *rpcError
		if !errors.As(err, &re) {
			re = classify(err)
		}
		resp.Error = re
		return resp
	}
	resp.Result = res
	return resp
}

// classify turns a wallet error into a bitcoin code.
func classify(err error) *rpcError {
	switch {
	case errors.Is(err, wallet.ErrNoFunds):
		return &rpcError{codeInsufficient, "Insufficient funds: " + err.Error()}
	case errors.Is(err, wallet.ErrBadAddress):
		return &rpcError{codeInvalidAddress, "Invalid address: " + err.Error()}
	case errors.Is(err, wallet.ErrAmountTooLow), errors.Is(err, wallet.ErrBadAmount):
		return &rpcError{codeInvalidParam, err.Error()}
	case errors.Is(err, wallet.ErrUnknownKey):
		return &rpcError{codeWalletError, "Private key for address is not known"}
	case errors.Is(err, nodeclient.ErrRPC):
		return &rpcError{codeMisc, err.Error()}
	}
	return &rpcError{codeMisc, err.Error()}
}

// ------------------------------------------------------------------ the parameters

// names are the parameter names in the order bitcoind takes them. A request may send an array
// (positionally) or an object (by name) — it is converted to an array.
var names = map[string][]string{
	"getnewaddress":                {"label", "address_type"},
	"getbalance":                   {"dummy", "minconf", "include_watchonly"},
	"listunspent":                  {"minconf", "maxconf", "addresses"},
	"sendtoaddress":                {"address", "amount", "comment", "comment_to", "subtractfeefromamount"},
	"sendmany":                     {"dummy", "amounts", "minconf", "comment"},
	"validateaddress":              {"address"},
	"getaddressinfo":               {"address"},
	"gettransaction":               {"txid", "include_watchonly"},
	"listtransactions":             {"label", "count", "skip", "include_watchonly"},
	"listsinceblock":               {"blockhash", "target_confirmations", "include_watchonly", "include_removed"},
	"dumpprivkey":                  {"address"},
	"importprivkey":                {"privkey", "label", "rescan"},
	"rescanblockchain":             {"start_height", "stop_height"},
	"setlabel":                     {"address", "label"},
	"getaddressesbylabel":          {"label"},
	"getreceivedbyaddress":         {"address", "minconf"},
	"getunconfirmedbalance":        {},
	"getbalances":                  {},
	"getwalletinfo":                {},
	"getrawchangeaddress":          {"address_type"},
	"createrawtransaction":         {"inputs", "outputs", "locktime", "replaceable"},
	"fundrawtransaction":           {"hexstring", "options", "iswitness"},
	"signrawtransactionwithwallet": {"hexstring", "prevtxs", "sighashtype"},
	"decoderawtransaction":         {"hexstring", "iswitness"},
	"sendrawtransaction":           {"hexstring", "maxfeerate"},
	"sweepall":                     {"address", "minconf", "maxinputs", "estimate"},
	"listreceivedbyaddress":        {"minconf", "include_empty", "include_watchonly", "address_filter"},
	"walletpassphrase":             {"passphrase", "timeout"},
	"walletlock":                   {},
	"walletpassphrasechange":       {"oldpassphrase", "newpassphrase"},
	"listwallets":                  {},
}

type args []json.RawMessage

func parseArgs(method string, raw json.RawMessage) (args, error) {
	t := strings.TrimSpace(string(raw))
	if t == "" || t == "null" {
		return nil, nil
	}
	if strings.HasPrefix(t, "[") {
		var a args
		if err := json.Unmarshal(raw, &a); err != nil {
			return nil, &rpcError{codeInvalidParam, "params: " + err.Error()}
		}
		return a, nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, &rpcError{codeInvalidParam, "params must be array or object"}
	}
	order := names[method]
	a := make(args, len(order))
	last := -1
	for i, n := range order {
		if v, ok := obj[n]; ok {
			a[i] = v
			last = i
			delete(obj, n)
		}
	}
	for n := range obj {
		return nil, &rpcError{codeInvalidParam, "unknown parameter " + n}
	}
	return a[:last+1], nil
}

func (a args) present(i int) bool {
	return i < len(a) && len(a[i]) > 0 && string(a[i]) != "null"
}

func (a args) str(i int, def string) (string, error) {
	if !a.present(i) {
		return def, nil
	}
	var s string
	if err := json.Unmarshal(a[i], &s); err != nil {
		return "", &rpcError{codeInvalidParam, fmt.Sprintf("parameter %d must be a string", i+1)}
	}
	return s, nil
}

func (a args) uint(i int, def uint64) (uint64, error) {
	if !a.present(i) {
		return def, nil
	}
	var v uint64
	if err := json.Unmarshal(a[i], &v); err != nil {
		return 0, &rpcError{codeInvalidParam, fmt.Sprintf("parameter %d must be a non-negative integer", i+1)}
	}
	return v, nil
}

func (a args) boolean(i int, def bool) (bool, error) {
	if !a.present(i) {
		return def, nil
	}
	var v bool
	if err := json.Unmarshal(a[i], &v); err != nil {
		return false, &rpcError{codeInvalidParam, fmt.Sprintf("parameter %d must be a boolean", i+1)}
	}
	return v, nil
}

// amountOf reads an amount from a number or a string — as text, without a float.
func amountOf(raw json.RawMessage) (int64, error) {
	t := strings.TrimSpace(string(raw))
	if strings.HasPrefix(t, `"`) {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return 0, wallet.ErrBadAmount
		}
		t = s
	}
	v, err := wallet.ParseAmount(t)
	if err != nil {
		return 0, &rpcError{codeInvalidParam, "Invalid amount " + t + " (POWM, up to 8 decimals)"}
	}
	if v <= 0 {
		return 0, &rpcError{codeInvalidParam, "Amount must be positive"}
	}
	return v, nil
}

// amt writes an amount as a JSON number with exactly eight decimal places.
func amt(cogs int64) json.Number { return json.Number(wallet.FormatAmount(cogs)) }

// ------------------------------------------------------------------ the methods

// forwarded are the chain methods wallet-rpc passes on to the node.
var forwarded = map[string]bool{
	"getinfo": true, "getblockcount": true, "getbestblockhash": true, "getblockhash": true,
	"getblock": true, "getrawtransaction": true, "getrawmempool": true,
	"gettxout": true, "getlanestats": true, "getfinality": true,
}

// walletless are the methods that do not need a wallet (they work on `/` with several loaded too).
var walletless = map[string]bool{"help": true, "listwallets": true, "createrawtransaction": true,
	"decoderawtransaction": true, "sendrawtransaction": true}

// route picks the wallet by the path (`/wallet/<name>`, or `/` when a single one is loaded).
func (s *Server) route(path, method string, raw json.RawMessage) (any, error) {
	if forwarded[method] {
		return s.forward(method, raw)
	}
	a, err := parseArgs(method, raw)
	if err != nil {
		return nil, err
	}
	var ctx *walletCtx
	switch {
	case strings.HasPrefix(path, "/wallet/"):
		l, name, ok := s.lookup(strings.TrimPrefix(path, "/wallet/"))
		if !ok {
			return nil, &rpcError{codeWalletNotFound, "Requested wallet does not exist or is not loaded"}
		}
		ctx = &walletCtx{Server: s, name: name, w: l.w, st: l.st}
	default:
		if l, name, ok := s.lookup(""); ok {
			ctx = &walletCtx{Server: s, name: name, w: l.w, st: l.st}
		}
	}
	if manageMethods[method] {
		if !s.AllowManage {
			// Without the flag the method pretends not to exist — not "you have no right".
			// The difference between "forbidden" and "does not exist" is extra information for
			// somebody probing what the server can do.
			return nil, &rpcError{codeMethodNotFound, "Method not found: " + method}
		}
		return s.manage(method, a)
	}
	if walletless[method] {
		return s.walletless(ctx, method, a)
	}
	if ctx == nil {
		return nil, &rpcError{codeWalletNotSpec, "Wallet file not specified (must request wallet RPC through /wallet/<filename> uri-path)."}
	}
	return ctx.dispatch(method, a)
}

func (s *walletCtx) dispatch(method string, a args) (any, error) {
	switch method {
	case "getnewaddress":
		label, err := a.str(0, "")
		if err != nil {
			return nil, err
		}
		return s.w.NewAddress(label)
	case "getbalance":
		// as in bitcoind: minconf 0 (the default) counts our own unconfirmed change too ("trusted")
		minconf, err := a.uint(1, 0)
		if err != nil {
			return nil, err
		}
		b := s.w.Balance(max(minconf, 1))
		if minconf == 0 {
			return amt(b.Available()), nil
		}
		return amt(b.Spendable), nil
	case "getunconfirmedbalance":
		return amt(0), nil // the wallet does not track foreign unconfirmed payments (it only sees blocks)
	case "getbalances":
		b := s.w.Balance(1)
		return map[string]any{"mine": map[string]any{"trusted": amt(b.Available()),
			"untrusted_pending": amt(0), "immature": amt(b.Immature), "locked": amt(b.Pending)}}, nil
	case "listunspent":
		return s.listUnspent(a)
	case "sendtoaddress":
		return s.sendToAddress(a)
	case "sendmany":
		return s.sendMany(a)
	case "validateaddress":
		return s.validateAddress(a)
	case "getaddressinfo":
		return s.addressInfo(a)
	case "gettransaction":
		return s.getTransaction(a)
	case "listtransactions":
		return s.listTransactions(a)
	case "listsinceblock":
		return s.listSinceBlock(a)
	case "getreceivedbyaddress":
		return s.receivedByAddress(a)
	case "listreceivedbyaddress":
		return s.listReceivedByAddress(a)
	case "dumpprivkey":
		addr, err := a.str(0, "")
		if err != nil {
			return nil, err
		}
		if err := s.needUnlocked(); err != nil {
			return nil, err
		}
		return s.w.DumpPrivKey(addr)
	case "importprivkey":
		return s.importPrivKey(a)
	case "rescanblockchain":
		from, err := a.uint(0, 0)
		if err != nil {
			return nil, err
		}
		if err := s.w.Rescan(from); err != nil {
			return nil, err
		}
		res, err := s.w.Sync(s.node)
		if err != nil {
			return nil, err
		}
		return map[string]any{"start_height": from, "stop_height": res.To}, nil
	case "setlabel":
		addr, err := a.str(0, "")
		if err != nil {
			return nil, err
		}
		label, err := a.str(1, "")
		if err != nil {
			return nil, err
		}
		if !s.w.IsMine(addr) {
			return nil, &rpcError{codeInvalidAddress, "Address is not in wallet"}
		}
		return nil, s.w.SetLabel(addr, label)
	case "getaddressesbylabel":
		label, err := a.str(0, "")
		if err != nil {
			return nil, err
		}
		out := map[string]any{}
		for _, ai := range s.w.Addresses() {
			if ai.Label == label {
				out[ai.Address] = map[string]string{"purpose": "receive"}
			}
		}
		return out, nil
	case "getwalletinfo":
		return s.walletInfo(), nil
	case "getrawchangeaddress":
		return s.w.ChangeAddress()
	case "fundrawtransaction":
		return s.fundRaw(a)
	case "signrawtransactionwithwallet":
		return s.signRaw(a)
	case "sweepall":
		return s.sweepAll(a)
	case "walletpassphrase":
		return s.walletPassphrase(a)
	case "walletlock":
		s.st.mu.Lock()
		s.st.until = time.Time{}
		s.st.mu.Unlock()
		return nil, nil
	case "walletpassphrasechange":
		return s.passphraseChange(a)
	}
	return nil, &rpcError{codeMethodNotFound, "Method not found: " + method}
}

func (s *Server) forward(method string, raw json.RawMessage) (any, error) {
	if s.fwd == nil {
		return nil, &rpcError{codeMethodNotFound, "Method not found: " + method}
	}
	var params any
	if len(raw) > 0 {
		params = raw
	}
	var out json.RawMessage
	ctx, cancel := contextTimeout()
	defer cancel()
	if err := s.fwd.Call(ctx, method, params, &out); err != nil {
		return nil, &rpcError{codeNodeUnavailable, err.Error()}
	}
	return out, nil
}

func (s *walletCtx) listUnspent(a args) (any, error) {
	minconf, err := a.uint(0, 1)
	if err != nil {
		return nil, err
	}
	maxconf, err := a.uint(1, 9_999_999)
	if err != nil {
		return nil, err
	}
	var filter map[string]bool
	if a.present(2) {
		var list []string
		if err := json.Unmarshal(a[2], &list); err != nil {
			return nil, &rpcError{codeInvalidParam, "addresses must be an array of strings"}
		}
		filter = make(map[string]bool, len(list))
		for _, x := range list {
			if _, err := s.w.DecodeAddress(x); err != nil {
				return nil, &rpcError{codeInvalidAddress, "Invalid address: " + x}
			}
			filter[x] = true
		}
	}
	out := []map[string]any{}
	for _, c := range s.w.Unspent(minconf, maxconf) {
		if filter != nil && !filter[c.Address] {
			continue
		}
		out = append(out, map[string]any{
			"txid": hex.EncodeToString(c.Outpoint.TxID[:]), "vout": c.Outpoint.Index,
			"address": c.Address, "label": c.Label, "amount": amt(c.Value),
			"confirmations": c.Confirmations, "spendable": c.Spendable, "solvable": true,
			"safe": c.Confirmations > 0, "coinbase": c.Coinbase,
		})
	}
	return out, nil
}

func (s *walletCtx) payment(addr string, amountRaw json.RawMessage) (wallet.Payment, error) {
	to, err := s.w.DecodeAddress(addr)
	if err != nil {
		return wallet.Payment{}, &rpcError{codeInvalidAddress, "Invalid address: " + addr}
	}
	v, err := amountOf(amountRaw)
	if err != nil {
		return wallet.Payment{}, err
	}
	return wallet.Payment{To: to, Amount: v}, nil
}

func (s *walletCtx) send(pays []wallet.Payment) (any, error) {
	if err := s.needUnlocked(); err != nil {
		return nil, err
	}
	s.st.sendMu.Lock()
	defer s.st.sendMu.Unlock()
	tx, _, err := s.w.Send(s.node, pays, s.FeePerKB)
	if err != nil {
		return nil, err
	}
	id := tx.TxID()
	return hex.EncodeToString(id[:]), nil
}

func (s *walletCtx) sendToAddress(a args) (any, error) {
	addr, err := a.str(0, "")
	if err != nil {
		return nil, err
	}
	if !a.present(1) {
		return nil, &rpcError{codeInvalidParam, "amount is required"}
	}
	if sub, _ := a.boolean(4, false); sub {
		return nil, &rpcError{codeInvalidParam, "subtractfeefromamount is not supported"}
	}
	p, err := s.payment(addr, a[1])
	if err != nil {
		return nil, err
	}
	return s.send([]wallet.Payment{p})
}

func (s *walletCtx) sendMany(a args) (any, error) {
	if !a.present(1) {
		return nil, &rpcError{codeInvalidParam, "amounts is required"}
	}
	var amounts map[string]json.RawMessage
	if err := json.Unmarshal(a[1], &amounts); err != nil || len(amounts) == 0 {
		return nil, &rpcError{codeInvalidParam, "amounts must be an object {address: amount}"}
	}
	addrs := make([]string, 0, len(amounts))
	for addr := range amounts {
		addrs = append(addrs, addr)
	}
	sort.Strings(addrs) // the output order is independent of the order in the JSON map
	pays := make([]wallet.Payment, 0, len(addrs))
	for _, addr := range addrs {
		p, err := s.payment(addr, amounts[addr])
		if err != nil {
			return nil, err
		}
		pays = append(pays, p)
	}
	return s.send(pays)
}

func (s *walletCtx) validateAddress(a args) (any, error) {
	addr, err := a.str(0, "")
	if err != nil {
		return nil, err
	}
	pk, err := s.w.DecodeAddress(addr)
	if err != nil {
		return map[string]any{"isvalid": false, "error": err.Error()}, nil
	}
	return map[string]any{"isvalid": true, "address": addr, "ismine": s.w.IsMine(addr),
		"iswatchonly": false, "isscript": false, "iswitness": true, "witness_version": chain.ScriptVersionBasic,
		"witness_program": hex.EncodeToString(pk[:])}, nil
}

func (s *walletCtx) addressInfo(a args) (any, error) {
	addr, err := a.str(0, "")
	if err != nil {
		return nil, err
	}
	if _, err := s.w.DecodeAddress(addr); err != nil {
		return nil, &rpcError{codeInvalidAddress, "Invalid address"}
	}
	mine := s.w.IsMine(addr)
	out := map[string]any{"address": addr, "ismine": mine, "iswatchonly": false, "solvable": mine}
	if l := s.w.Label(addr); l != "" {
		out["labels"] = []string{l}
	} else {
		out["labels"] = []string{}
	}
	return out, nil
}

// entries splits a transaction into history entries as bitcoind does (receive/send/generate/immature).
// Change to our own change address is not shown.
func (s *walletCtx) entries(t wallet.TxView) []map[string]any {
	st := s.w.Status()
	base := func() map[string]any {
		m := map[string]any{"txid": hex.EncodeToString(t.ID[:]), "confirmations": t.Confirmations,
			"time": t.Time, "timereceived": t.Time}
		if t.Height > 0 {
			m["blockhash"] = hex.EncodeToString(t.BlockID[:])
			m["blockheight"] = t.Height
			m["blocktime"] = t.Time
		}
		return m
	}
	var out []map[string]any
	if t.Spent > 0 {
		for _, o := range t.Outputs {
			if o.Mine {
				continue // change is not shown; a payment to ourselves shows up as a receive below
			}
			m := base()
			m["category"] = "send"
			m["address"] = o.Address
			m["amount"] = amt(-o.Value)
			m["vout"] = o.Index
			if t.Fee >= 0 {
				m["fee"] = amt(-t.Fee)
			}
			m["abandoned"] = false
			out = append(out, m)
		}
	}
	for _, o := range t.Outputs {
		if !o.Mine || o.Change {
			continue
		}
		m := base()
		m["address"] = o.Address
		m["amount"] = amt(o.Value)
		m["vout"] = o.Index
		if l := s.w.Label(o.Address); l != "" {
			m["label"] = l
		}
		switch {
		case !t.Coinbase:
			m["category"] = "receive"
		case t.Height > 0 && st.Height >= t.Height+chain.CoinbaseMaturity:
			m["category"] = "generate"
		default:
			m["category"] = "immature"
		}
		out = append(out, m)
	}
	return out
}

func (s *walletCtx) txid(a args, i int) ([32]byte, error) {
	str, err := a.str(i, "")
	if err != nil {
		return [32]byte{}, err
	}
	b, err := hex.DecodeString(str)
	if err != nil || len(b) != 32 {
		return [32]byte{}, &rpcError{codeInvalidParam, "txid must be 64 hex characters"}
	}
	var id [32]byte
	copy(id[:], b)
	return id, nil
}

func (s *walletCtx) getTransaction(a args) (any, error) {
	id, err := s.txid(a, 0)
	if err != nil {
		return nil, err
	}
	t, ok := s.w.Transaction(id)
	if !ok {
		return nil, &rpcError{codeInvalidAddress, "Invalid or non-wallet transaction id"}
	}
	net := t.Net()
	if t.Spent > 0 && t.Fee >= 0 {
		net += t.Fee // bitcoind: amount is without the fee, the fee separately
	}
	out := map[string]any{"txid": hex.EncodeToString(t.ID[:]), "amount": amt(net),
		"confirmations": t.Confirmations, "time": t.Time, "timereceived": t.Time,
		"details": s.entries(t)}
	if t.Spent > 0 && t.Fee >= 0 {
		out["fee"] = amt(-t.Fee)
	}
	if t.Height > 0 {
		out["blockhash"] = hex.EncodeToString(t.BlockID[:])
		out["blockheight"] = t.Height
		out["blocktime"] = t.Time
	}
	if t.Raw != nil {
		out["hex"] = hex.EncodeToString(t.Raw)
	}
	return out, nil
}

func (s *walletCtx) listTransactions(a args) (any, error) {
	label, err := a.str(0, "*")
	if err != nil {
		return nil, err
	}
	count, err := a.uint(1, 10)
	if err != nil {
		return nil, err
	}
	skip, err := a.uint(2, 0)
	if err != nil {
		return nil, err
	}
	var all []map[string]any
	for _, t := range s.w.Transactions() { // from the newest
		for _, e := range s.entries(t) {
			if label != "*" && e["label"] != label {
				continue
			}
			all = append(all, e)
		}
	}
	// bitcoind: skip the `skip` newest, take `count`, and return them from the oldest
	if skip >= uint64(len(all)) {
		return []map[string]any{}, nil
	}
	all = all[skip:]
	if count < uint64(len(all)) {
		all = all[:count]
	}
	for i, j := 0, len(all)-1; i < j; i, j = i+1, j-1 {
		all[i], all[j] = all[j], all[i]
	}
	return all, nil
}

// listSinceBlock: everything after the block `blockhash` (excluding it) + the unconfirmed ones. A block
// the wallet does not know (too old, or from a disconnected branch) means "from the beginning" — the
// same as bitcoind, so an adapter misses nothing after a reorg.
func (s *walletCtx) listSinceBlock(a args) (any, error) {
	var since uint64
	hasSince := false
	if a.present(0) {
		id, err := s.txid(a, 0)
		if err != nil {
			return nil, err
		}
		if h, ok := s.w.HeightOf(id); ok {
			since, hasSince = h, true
		}
	}
	target, err := a.uint(1, 1)
	if err != nil {
		return nil, err
	}
	txs := []map[string]any{}
	for _, t := range s.w.Transactions() {
		if hasSince && t.Height != 0 && t.Height <= since {
			continue
		}
		txs = append(txs, s.entries(t)...)
	}
	st := s.w.Status()
	lastH := uint64(0)
	if st.Height+1 > target {
		lastH = st.Height + 1 - max(target, 1)
	}
	last, _ := s.w.BlockAt(lastH)
	return map[string]any{"transactions": txs, "removed": []any{},
		"lastblock": hex.EncodeToString(last[:])}, nil
}

func (s *walletCtx) receivedByAddress(a args) (any, error) {
	addr, err := a.str(0, "")
	if err != nil {
		return nil, err
	}
	if _, err := s.w.DecodeAddress(addr); err != nil {
		return nil, &rpcError{codeInvalidAddress, "Invalid address"}
	}
	minconf, err := a.uint(1, 1)
	if err != nil {
		return nil, err
	}
	var total int64
	for _, t := range s.w.Transactions() {
		if t.Confirmations < minconf {
			continue
		}
		for _, o := range t.Outputs {
			if o.Address == addr && o.Mine {
				total += o.Value
			}
		}
	}
	return amt(total), nil
}

// listReceivedByAddress lists the wallet's own addresses with what each has received, in bitcoind's shape.
// With include_empty it is also the only way to ask "which addresses do I have": a wallet UI needs that to
// show a person their addresses and labels.
func (s *walletCtx) listReceivedByAddress(a args) (any, error) {
	minconf, err := a.uint(0, 1)
	if err != nil {
		return nil, err
	}
	includeEmpty, err := a.boolean(1, false)
	if err != nil {
		return nil, err
	}
	type received struct {
		total int64
		conf  uint64
		txids []string
	}
	got := map[string]*received{}
	for _, t := range s.w.Transactions() { // from the newest
		if t.Confirmations < minconf {
			continue
		}
		id := hex.EncodeToString(t.ID[:])
		for _, o := range t.Outputs {
			if !o.Mine || o.Change {
				continue // change coming back is not something the address "received"
			}
			r := got[o.Address]
			if r == nil {
				r = &received{conf: t.Confirmations} // of the most recent payment, as in bitcoind
				got[o.Address] = r
			}
			r.total += o.Value
			if n := len(r.txids); n == 0 || r.txids[n-1] != id {
				r.txids = append(r.txids, id)
			}
		}
	}
	out := []map[string]any{}
	for _, ai := range s.w.Addresses() {
		r := got[ai.Address]
		if r == nil {
			if !includeEmpty {
				continue
			}
			r = &received{txids: []string{}}
		}
		out = append(out, map[string]any{"address": ai.Address, "amount": amt(r.total),
			"confirmations": r.conf, "label": ai.Label, "txids": r.txids, "imported": ai.Imported})
	}
	return out, nil
}

func (s *walletCtx) importPrivKey(a args) (any, error) {
	key, err := a.str(0, "")
	if err != nil {
		return nil, err
	}
	label, err := a.str(1, "")
	if err != nil {
		return nil, err
	}
	rescan, err := a.boolean(2, true)
	if err != nil {
		return nil, err
	}
	if err := s.needUnlocked(); err != nil {
		return nil, err
	}
	_, need, err := s.w.ImportPrivKey(key, label, 0)
	if err != nil {
		return nil, &rpcError{codeInvalidKey, "Invalid private key encoding: " + err.Error()}
	}
	if need && rescan {
		if err := s.w.Rescan(0); err != nil {
			return nil, err
		}
		if _, err := s.w.Sync(s.node); err != nil {
			return nil, err
		}
	}
	return nil, nil
}

func (s *walletCtx) walletInfo() map[string]any {
	st := s.w.Status()
	b := s.w.Balance(1)
	out := map[string]any{"walletname": s.name, "walletversion": 1, "format": "powmachina",
		"balance": amt(b.Available()), "unconfirmed_balance": amt(0),
		"immature_balance": amt(b.Immature), "locked_balance": amt(b.Pending),
		"txcount": len(s.w.Transactions()), "height": st.Height, "synced": st.Synced,
		"birth_height": st.Birth, "addresses": st.Addresses, "imported_keys": st.Imported,
		"private_keys_enabled": true}
	if s.RequirePassphrase {
		out["unlocked_until"] = s.st.unlockedUntil()
	}
	return out
}

func (s *Server) help() string {
	var ms []string
	for m := range names {
		ms = append(ms, m)
	}
	for m := range walletless {
		ms = append(ms, m)
	}
	for m := range forwarded {
		ms = append(ms, m+" (node)")
	}
	sort.Strings(ms)
	return "powmachina-wallet-rpc methods:\n  " + strings.Join(ms, "\n  ")
}
