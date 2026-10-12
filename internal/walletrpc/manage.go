package walletrpc

import (
	"os"
	"path/filepath"
	"strings"

	"powmachina/internal/chain"
	"powmachina/internal/wallet"
)

// Wallet management: create, restore from the words, open, close.
//
// This is **for the GUI**, not for an exchange. An exchange gets its wallets through `-wallet-file` at
// startup and never needs RPC to be able to touch files on disk. That is why these methods sit behind
// the `-allow-manage` flag and pretend not to exist without it: whoever obtains an exchange's RPC token
// must not be able to create or open a wallet at an arbitrary path.
//
// The GUI (Tauri) starts wallet-rpc itself, on 127.0.0.1, with a random token for that one run — there
// the flag is on.

// manageMethods are the management methods. They are `walletless` — they do not need a loaded wallet,
// because they are the ones about to create or open it.
var manageMethods = map[string]bool{
	"createwallet": true, "restorewallet": true, "loadwallet": true,
	"unloadwallet": true, "getseed": true,
}

// walletName is the name a wallet lives under in `/wallet/<name>`: the file name without its path.
// Two files with the same name in different directories would otherwise overlap.
func walletName(path string) string {
	return strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
}

// manage handles the management methods. `AllowManage` is checked by the caller.
func (s *Server) manage(method string, a args) (any, error) {
	switch method {
	case "createwallet":
		return s.createWallet(a)
	case "restorewallet":
		return s.restoreWallet(a)
	case "loadwallet":
		return s.loadWallet(a)
	case "unloadwallet":
		return s.unloadWallet(a)
	case "getseed":
		return s.getSeed(a)
	}
	return nil, &rpcError{codeMethodNotFound, "Method not found: " + method}
}

// createWallet creates a new wallet. It returns the **24 words** — the only time the server hands them
// out on its own. The caller has to show them to a person and store them nowhere.
func (s *Server) createWallet(a args) (any, error) {
	path, err := a.str(0, "")
	if err != nil {
		return nil, err
	}
	pass, err := a.str(1, "")
	if err != nil {
		return nil, err
	}
	if path == "" || pass == "" {
		return nil, &rpcError{codeInvalidParam, "createwallet requires a path and a password"}
	}
	// Overwriting an existing wallet would mean deleting keys. Never.
	if _, err := os.Stat(path); err == nil {
		return nil, &rpcError{codeWalletError, "the file already exists: " + path}
	}
	birth := s.tipHeight()
	w, mnemonic, err := wallet.Create(path, pass, s.params(), birth)
	if err != nil {
		return nil, &rpcError{codeWalletError, err.Error()}
	}
	name := walletName(path)
	if err := s.AddWallet(name, w); err != nil {
		return nil, &rpcError{codeWalletError, err.Error()}
	}
	addr, err := w.NewAddress("")
	if err != nil {
		return nil, &rpcError{codeWalletError, err.Error()}
	}
	return map[string]any{"name": name, "address": addr, "mnemonic": mnemonic,
		"birth_height": birth}, nil
}

// restoreWallet restores a wallet from 24 words. `birth` is the height to scan from — without it the
// whole history is scanned, which is right for a restore, it just takes time.
func (s *Server) restoreWallet(a args) (any, error) {
	path, err := a.str(0, "")
	if err != nil {
		return nil, err
	}
	pass, err := a.str(1, "")
	if err != nil {
		return nil, err
	}
	mnemonic, err := a.str(2, "")
	if err != nil {
		return nil, err
	}
	birth, err := a.uint(3, 0)
	if err != nil {
		return nil, err
	}
	if path == "" || pass == "" || mnemonic == "" {
		return nil, &rpcError{codeInvalidParam, "restorewallet requires a path, a password and the words"}
	}
	if _, err := os.Stat(path); err == nil {
		return nil, &rpcError{codeWalletError, "the file already exists: " + path}
	}
	w, err := wallet.Restore(path, pass, mnemonic, s.params(), birth)
	if err != nil {
		return nil, &rpcError{codeWalletError, err.Error()}
	}
	name := walletName(path)
	if err := s.AddWallet(name, w); err != nil {
		return nil, &rpcError{codeWalletError, err.Error()}
	}
	// The first address is derived right away: after a restore a person wants to see that it is **the
	// same** wallet. An empty address list would look as if something else had been restored.
	addr, err := w.NewAddress("")
	if err != nil {
		return nil, &rpcError{codeWalletError, err.Error()}
	}
	return map[string]any{"name": name, "address": addr, "birth_height": birth}, nil
}

// loadWallet opens an existing file. A wrong password is `codeWrongPassphrase`, not "a file error" —
// the GUI knows from that whether to ask again.
func (s *Server) loadWallet(a args) (any, error) {
	path, err := a.str(0, "")
	if err != nil {
		return nil, err
	}
	pass, err := a.str(1, "")
	if err != nil {
		return nil, err
	}
	if path == "" {
		return nil, &rpcError{codeInvalidParam, "loadwallet requires a path"}
	}
	name := walletName(path)
	if _, _, dup := s.lookup(name); dup {
		return map[string]any{"name": name, "already_loaded": true}, nil
	}
	w, err := wallet.Open(path, pass, s.params())
	if err != nil {
		if strings.Contains(err.Error(), "password") || strings.Contains(err.Error(), "password") {
			return nil, &rpcError{codeWrongPassphrase, err.Error()}
		}
		return nil, &rpcError{codeWalletError, err.Error()}
	}
	if err := s.AddWallet(name, w); err != nil {
		return nil, &rpcError{codeWalletError, err.Error()}
	}
	return map[string]any{"name": name}, nil
}

// unloadWallet closes a wallet. The file stays, it just stops being served.
func (s *Server) unloadWallet(a args) (any, error) {
	name, err := a.str(0, "")
	if err != nil {
		return nil, err
	}
	l, name, ok := s.lookup(name)
	if !ok {
		return nil, &rpcError{codeWalletNotFound, "Requested wallet does not exist or is not loaded"}
	}
	// Closing (saving) has to happen before the wallet disappears from the list — otherwise a request for
	// a wallet that is being saved could arrive in the meantime.
	s.mu.Lock()
	delete(s.wallets, name)
	for i, n := range s.order {
		if n == name {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
	s.mu.Unlock()
	if err := l.w.Close(); err != nil {
		return nil, &rpcError{codeWalletError, err.Error()}
	}
	return map[string]any{"name": name}, nil
}

// getSeed hands out the 24 words for a backup or a paper wallet. **It requires the password** even when
// the wallet is unlocked: the words are the whole wallet forever, not just the right to spend once.
func (s *Server) getSeed(a args) (any, error) {
	name, err := a.str(0, "")
	if err != nil {
		return nil, err
	}
	pass, err := a.str(1, "")
	if err != nil {
		return nil, err
	}
	l, _, ok := s.lookup(name)
	if !ok {
		return nil, &rpcError{codeWalletNotFound, "Requested wallet does not exist or is not loaded"}
	}
	if pass == "" {
		return nil, &rpcError{codeInvalidParam, "getseed requires the wallet's password"}
	}
	// The same limit on wrong attempts as walletpassphrase: it is the same password.
	right, err := l.st.tryPassword(func() bool { return l.w.CheckPassword(pass) })
	if err != nil {
		return nil, err
	}
	if !right {
		return nil, &rpcError{codeWrongPassphrase, "Error: The wallet passphrase entered was incorrect."}
	}
	addrs := l.w.Addresses()
	first := ""
	if len(addrs) > 0 {
		first = addrs[0].Address
	}
	return map[string]any{"mnemonic": l.w.Mnemonic(), "address": first,
		"network": l.w.Params().Name}, nil
}

// params is the network the server runs on. It is taken from the first loaded wallet — a new wallet has
// to be created on the same network as the ones already running. When there is none yet (the GUI before
// the first one is created), `Params` from the startup decides.
//
// A default of mainnet would be dangerous here: a wallet created "by mistake on mainnet" looks the same,
// only nobody will pay its addresses on testnet.
func (s *Server) params() chain.NetParams {
	// Deliberately the **first** loaded one, not "the only one": with several wallets they are all from the
	// same network (AddWallet guards that), so the first one is enough. `lookup("")` would be wrong here —
	// it returns nothing when there are several wallets, and the network would fall back to the default.
	s.mu.RLock()
	if len(s.order) > 0 {
		p := s.wallets[s.order[0]].w.Params()
		s.mu.RUnlock()
		return p
	}
	s.mu.RUnlock()
	if s.Params.Magic == ([4]byte{}) {
		return chain.MainNet
	}
	return s.Params
}

// tipHeight is the height it makes sense to scan a new wallet from: it does not care about history.
// When the node is unreachable it returns 0 — better to scan needlessly than to miss a payment.
func (s *Server) tipHeight() uint64 {
	if s.node == nil {
		return 0
	}
	h, _, err := s.node.Tip()
	if err != nil {
		return 0
	}
	return h
}
