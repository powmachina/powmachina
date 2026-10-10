package walletrpc

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"powmachina/internal/chain"
	"powmachina/internal/wallet"
	"powmachina/internal/wallet/wallettest"
)

// manageEnv is a server **without** a loaded wallet and with management on — exactly how the GUI starts
// it when a person does not have a wallet yet.
func manageEnv(t *testing.T) (*env, string) {
	t.Helper()
	n := wallettest.NewNode()
	s := New(n, nil, token)
	s.AllowManage = true
	s.Params = chain.RegTest
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	// The directory first, the closing second: cleanups run last-in-first-out, and a wallet that is still
	// open holds its lock file — on Windows that file cannot be removed while it is held.
	dir := t.TempDir()
	t.Cleanup(func() { closeWallets(s) })
	return &env{t: t, node: n, srv: srv}, dir
}

// closeWallets closes whatever the test left loaded, so that the lock files can be removed.
func closeWallets(s *Server) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, l := range s.wallets {
		l.w.Close()
	}
}

func TestCreateLoadAndUnloadWallet(t *testing.T) {
	e, dir := manageEnv(t)
	path := filepath.Join(dir, "mywallet.wallet")

	var created struct {
		Name     string `json:"name"`
		Address  string `json:"address"`
		Mnemonic string `json:"mnemonic"`
	}
	if err := json.Unmarshal(e.ok("createwallet", path, "password"), &created); err != nil {
		t.Fatal(err)
	}
	if created.Name != "mywallet" {
		t.Errorf("the name is %q, expected the file name without its extension", created.Name)
	}
	if n := len(strings.Fields(created.Mnemonic)); n != 24 {
		t.Fatalf("expected 24 words, got %d", n)
	}
	// The address has to be from the network the server runs on — a wallet "by mistake on mainnet" looks
	// the same, only nobody will pay it on testnet.
	if !strings.HasPrefix(created.Address, chain.RegTest.Bech32HRP+"1") {
		t.Errorf("address %q is not a regtest one", created.Address)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the wallet file was not created: %v", err)
	}

	// Creating it a second time at the same path would overwrite the keys — it has to fail.
	if r := e.call("createwallet", path, "password"); r.Error == nil {
		t.Error("an existing file could be overwritten")
	}

	// The words can be asked for again, but only with the password.
	if r := e.call("getseed", "mywallet", "wrong-pass"); r.Error == nil || r.Error.Code != codeWrongPassphrase {
		t.Errorf("a wrong password should give codeWrongPassphrase, it gave %v", r.Error)
	}
	var seed struct {
		Mnemonic string `json:"mnemonic"`
	}
	if err := json.Unmarshal(e.ok("getseed", "mywallet", "password"), &seed); err != nil {
		t.Fatal(err)
	}
	if seed.Mnemonic != created.Mnemonic {
		t.Error("getseed returned different words than createwallet")
	}

	// Close and open again — with the password.
	e.ok("unloadwallet", "mywallet")
	if r := e.call("getbalance"); r.Error == nil {
		t.Error("getbalance could be called after unloadwallet")
	}
	if r := e.call("loadwallet", path, "wrong"); r.Error == nil {
		t.Error("the wallet opened with a wrong password")
	}
	e.ok("loadwallet", path, "password")
	if got := string(e.ok("listwallets")); got != `["mywallet"]` {
		t.Errorf("listwallets = %s", got)
	}
}

// A restore from the words has to give the same first address — otherwise a 24-word backup is no backup.
func TestRestoreWalletGivesSameAddress(t *testing.T) {
	e, dir := manageEnv(t)
	var created struct {
		Address  string `json:"address"`
		Mnemonic string `json:"mnemonic"`
	}
	if err := json.Unmarshal(e.ok("createwallet", filepath.Join(dir, "a.wallet"), "password"), &created); err != nil {
		t.Fatal(err)
	}
	e.ok("unloadwallet", "a")

	var restored struct {
		Address string `json:"address"`
	}
	if err := json.Unmarshal(e.ok("restorewallet", filepath.Join(dir, "b.wallet"),
		"other-password", created.Mnemonic, 0), &restored); err != nil {
		t.Fatal(err)
	}
	if restored.Address != created.Address {
		t.Fatalf("the restored wallet's first address is %s, the original is %s", restored.Address, created.Address)
	}
	// The file's password is new — the words and the password are two different things.
	if r := e.call("getseed", "b", "password"); r.Error == nil {
		t.Error("the restored wallet opened with the original password")
	}
}

// Without the flag the management methods have to pretend they **do not exist**. An exchange gets its
// wallets through --wallet-file and its RPC token must not be enough to create or open a wallet at an
// arbitrary path.
func TestManageMethodsHiddenWithoutFlag(t *testing.T) {
	e := setup(t) // an ordinary server, AllowManage = false
	dir := t.TempDir()
	for _, c := range []struct {
		method string
		params []any
	}{
		{"createwallet", []any{filepath.Join(dir, "x"), "password"}},
		{"restorewallet", []any{filepath.Join(dir, "y"), "password", "words", 0}},
		{"loadwallet", []any{filepath.Join(dir, "z"), "password"}},
		{"unloadwallet", []any{"exchange"}},
		{"getseed", []any{"exchange", "password"}},
	} {
		r := e.call(c.method, c.params...)
		if r.Error == nil || r.Error.Code != codeMethodNotFound {
			t.Errorf("%s: expected \"the method does not exist\", got %v", c.method, r.Error)
		}
	}
	// And the wallet stayed loaded — unloadwallet did nothing.
	if got := string(e.ok("listwallets")); got != `["exchange"]` {
		t.Errorf("listwallets = %s", got)
	}
}

// A wallet added while running has to get a sync loop, otherwise it would pretend nothing ever landed
// on it.
func TestOnWalletAddedFiresForRuntimeWallets(t *testing.T) {
	n := wallettest.NewNode()
	s := New(n, nil, token)
	s.AllowManage = true
	s.Params = chain.RegTest
	added := make(chan string, 4)
	s.OnWalletAdded = func(name string, _ *wallet.Wallet) { added <- name }
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	e := &env{t: t, node: n, srv: srv}
	dir := t.TempDir()
	t.Cleanup(func() { closeWallets(s) })

	e.ok("createwallet", filepath.Join(dir, "newone.wallet"), "password")
	select {
	case name := <-added:
		if name != "newone" {
			t.Errorf("the hook got the name %q", name)
		}
	default:
		t.Fatal("OnWalletAdded was not called — the wallet would not scan blocks")
	}
}
