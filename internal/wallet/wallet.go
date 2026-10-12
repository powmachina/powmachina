// Package wallet is the PowMachina wallet: 24 words, one encrypted wallet.dat file and block
// scanning of its own.
//
// The architecture is like Monero's: **the node has no keys.** The wallet (powmachina-wallet-cli for
// people, powmachina-wallet-rpc for exchanges and services) connects to a node, downloads blocks and
// finds its own coins in them itself. The node therefore does not learn which addresses are yours,
// and the process that talks to the internet has nothing to steal.
//
// The file holds the keys and the cache (coins, history, how far the wallet is synced), all under one
// password — the format is in file.go and the wallet spec.
package wallet

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"

	"powmachina/internal/address"
	"powmachina/internal/bip39"
	"powmachina/internal/chain"
)

// payloadVersion is the version of the content (inside the encryption). It is raised when the structure
// changes; older content is converted on load.
const payloadVersion = 1

var (
	ErrExists       = errors.New("wallet: file already exists — refusing to overwrite it")
	ErrClosed       = errors.New("wallet: the wallet is closed")
	ErrUnknownKey   = errors.New("wallet: the wallet does not have the key for this address")
	ErrBadAddress   = errors.New("wallet: invalid address")
	ErrNoFunds      = errors.New("wallet: insufficient funds")
	ErrAmountTooLow = errors.New("wallet: amount is below the dust limit")
)

// Coin is a wallet's coin.
type Coin struct {
	Outpoint    chain.Outpoint
	Value       int64
	PubKey      [32]byte
	Height      uint64 // the block it was created in; 0 = an output of our unconfirmed transaction
	Coinbase    bool
	SpentBy     [32]byte // a confirmed spend (zero = none)
	SpentHeight uint64
	PendingBy   [32]byte // a spend by our unconfirmed transaction
}

func (c *Coin) spent() bool   { return c.SpentHeight != 0 }
func (c *Coin) pending() bool { return c.PendingBy != [32]byte{} }

// Output is a transaction's output as the wallet sees it.
type Output struct {
	Index   uint32
	Address string
	Value   int64
	Mine    bool
	Change  bool `json:",omitempty"` // change to our own change address — not shown in the history
}

// Tx is a transaction that touched the wallet.
type Tx struct {
	ID       [32]byte
	Height   uint64 // 0 = unconfirmed (one we sent)
	BlockID  [32]byte
	Time     int64
	Received int64 // the sum of our outputs
	Spent    int64 // the sum of our spent inputs
	Fee      int64 // -1 = unknown (not every input is ours)
	Coinbase bool
	Outputs  []Output
	Raw      []byte           // only ones we sent — for rebroadcasting
	Inputs   []chain.Outpoint // only ones we sent — which coins it reserves
}

// Net is the change of balance the transaction caused.
func (t *Tx) Net() int64 { return t.Received - t.Spent }

type imported struct {
	Key   [32]byte
	Birth uint64
}

type blockRef struct {
	Height uint64
	ID     [32]byte
}

// Wallet is an open wallet. Its methods are safe to call concurrently.
type Wallet struct {
	mu     sync.Mutex
	syncMu sync.Mutex // one sync at a time (the background loop and a rescan from RPC)
	path   string
	params chain.NetParams
	lock   *fileLock

	kdf  KDFParams
	salt [saltBytes]byte
	key  []byte // the file's key (from Argon2id), kept in memory for saving
	// closed: Close has run, the key is gone and the file lock is released. Nothing may be written
	// from here on — a save would seal the file with the zeroed key.
	closed bool

	entropy    []byte
	keys       *keychain
	birth      uint64
	nextRecv   uint32
	nextChange uint32
	imported   map[[32]byte]imported
	labels     map[string]string // address → label
	changeTo   *[32]byte         // a fixed change address (SetChangeAddress); nil = a new one every time

	// the sync cache
	synced bool
	height uint64
	tip    [32]byte
	recent []blockRef // the last ReorgLimit+1 blocks — for finding the common ancestor during a reorg
	coins  map[chain.Outpoint]*Coin
	txs    map[[32]byte]*Tx
}

// Create creates a new wallet with new 24 words. `birth` is the height it is worth scanning from (the
// node's current tip — a new wallet has nothing to look for in history).
func Create(path, password string, params chain.NetParams, birth uint64) (*Wallet, string, error) {
	entropy := make([]byte, 32)
	if _, err := rand.Read(entropy); err != nil {
		return nil, "", err
	}
	mnemonic, err := bip39.NewMnemonic(entropy)
	if err != nil {
		return nil, "", err
	}
	w, err := create(path, password, params, entropy, birth)
	if err != nil {
		return nil, "", err
	}
	return w, mnemonic, nil
}

// Restore restores a wallet from its words. `birth` is the height to scan from (0 = the whole history).
func Restore(path, password, mnemonic string, params chain.NetParams, birth uint64) (*Wallet, error) {
	entropy, err := bip39.Entropy(mnemonic)
	if err != nil {
		return nil, err
	}
	return create(path, password, params, entropy, birth)
}

func create(path, password string, params chain.NetParams, entropy []byte, birth uint64) (*Wallet, error) {
	if password == "" {
		return nil, ErrNoPassword
	}
	if _, err := os.Stat(path); err == nil {
		return nil, ErrExists
	}
	lk, err := lockFile(path)
	if err != nil {
		return nil, err
	}
	w := newWallet(path, params, lk)
	if err := w.initKeys(entropy); err != nil {
		lk.unlock()
		return nil, err
	}
	w.birth = birth
	w.kdf = DefaultKDF
	if w.salt, err = newSalt(); err != nil {
		lk.unlock()
		return nil, err
	}
	w.key = deriveKey(password, w.salt[:], w.kdf)
	if err := w.saveLocked(); err != nil {
		lk.unlock()
		return nil, err
	}
	return w, nil
}

func newWallet(path string, params chain.NetParams, lk *fileLock) *Wallet {
	return &Wallet{path: path, params: params, lock: lk,
		imported: make(map[[32]byte]imported), labels: make(map[string]string),
		coins: make(map[chain.Outpoint]*Coin), txs: make(map[[32]byte]*Tx)}
}

func (w *Wallet) initKeys(entropy []byte) error {
	mnemonic, err := bip39.NewMnemonic(entropy)
	if err != nil {
		return err
	}
	seed, err := bip39.Seed(mnemonic, "")
	if err != nil {
		return err
	}
	w.keys, err = newKeychain(seed)
	if err != nil {
		return err
	}
	w.entropy = append([]byte{}, entropy...)
	return w.extendLocked()
}

// extendLocked derives the keys up to the lookahead above the used indices.
func (w *Wallet) extendLocked() error {
	if err := w.keys.ensure(branchReceive, w.nextRecv+receiveLookahead); err != nil {
		return err
	}
	return w.keys.ensure(branchChange, w.nextChange+changeLookahead)
}

// Open opens an existing wallet. The file is locked — a second process will not open it.
func Open(path, password string, params chain.NetParams) (*Wallet, error) {
	lk, err := lockFile(path)
	if err != nil {
		return nil, err
	}
	w, err := openLocked(path, password, params, lk)
	if err != nil {
		lk.unlock()
		return nil, err
	}
	return w, nil
}

func openLocked(path, password string, params chain.NetParams, lk *fileLock) (*Wallet, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	f, err := parseFile(data)
	if err != nil {
		return nil, err
	}
	if f.network != params.Magic {
		return nil, netErr(params.Magic, f.network)
	}
	key := deriveKey(password, f.salt[:], f.kdf)
	plain, err := f.open(key)
	if err != nil {
		return nil, err
	}
	w := newWallet(path, params, lk)
	w.kdf, w.salt, w.key = f.kdf, f.salt, key
	if err := w.loadPayload(plain); err != nil {
		return nil, err
	}
	return w, nil
}

// Close saves the state and releases the lock. The wallet is of no use afterwards: whoever still holds
// it (a sync loop, a request in flight) gets ErrClosed instead of a save. A second Close does nothing.
func (w *Wallet) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	err := w.saveLocked()
	w.closed = true
	w.lock.unlock()
	for i := range w.key {
		w.key[i] = 0
	}
	return err
}

// Save writes the wallet out (atomically).
func (w *Wallet) Save() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.saveLocked()
}

func (w *Wallet) saveLocked() error {
	if w.closed {
		return ErrClosed
	}
	plain, err := w.marshalPayload()
	if err != nil {
		return err
	}
	data, err := seal(w.params.Magic, w.kdf, w.salt, w.key, plain)
	for i := range plain {
		plain[i] = 0
	}
	if err != nil {
		return err
	}
	return writeAtomic(w.path, data)
}

// CheckPassword verifies the password (for instance before showing the words or a key). It costs one Argon2id.
func (w *Wallet) CheckPassword(password string) bool {
	w.mu.Lock()
	salt, kdf, key := w.salt, w.kdf, w.key
	w.mu.Unlock()
	return subtle.ConstantTimeCompare(deriveKey(password, salt[:], kdf), key) == 1
}

// ChangePassword re-encrypts the file with a new password (a new salt, a new key). The old password is verified.
func (w *Wallet) ChangePassword(oldPassword, newPassword string) error {
	if newPassword == "" {
		return ErrNoPassword
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if subtle.ConstantTimeCompare(deriveKey(oldPassword, w.salt[:], w.kdf), w.key) != 1 {
		return ErrWrongPassword
	}
	salt, err := newSalt()
	if err != nil {
		return err
	}
	w.kdf, w.salt = DefaultKDF, salt
	w.key = deriveKey(newPassword, salt[:], w.kdf)
	return w.saveLocked()
}

// Mnemonic returns the 24 words. Whoever has them has every derived address (not the imported ones).
func (w *Wallet) Mnemonic() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	m, _ := bip39.NewMnemonic(w.entropy)
	return m
}

// Params is the wallet's network.
func (w *Wallet) Params() chain.NetParams { return w.params }

// Path is the wallet's file.
func (w *Wallet) Path() string { return w.path }

// ------------------------------------------------------------------ addresses and keys

func (w *Wallet) encode(pub [32]byte) string {
	a, _ := address.Encode(w.params.Bech32HRP, chain.ScriptVersionBasic, pub[:])
	return a
}

// DecodeAddressLocked is DecodeAddress for a caller that holds the lock (DecodeAddress does not take
// the lock, so they are identical — the name only says that this is fine).
func (w *Wallet) DecodeAddressLocked(addr string) ([32]byte, error) { return w.DecodeAddress(addr) }

// DecodeAddress decodes an address of this network into the recipient's key.
func (w *Wallet) DecodeAddress(addr string) ([32]byte, error) {
	v, prog, err := address.Decode(w.params.Bech32HRP, addr)
	if err != nil {
		return [32]byte{}, fmt.Errorf("%w: %w", ErrBadAddress, err)
	}
	if v != chain.ScriptVersionBasic || len(prog) != chain.PubkeyBytes {
		return [32]byte{}, fmt.Errorf("%w: unsupported address type", ErrBadAddress)
	}
	var pk [32]byte
	copy(pk[:], prog)
	return pk, nil
}

// NewAddress issues the next receiving address and optionally gives it a label.
func (w *Wallet) NewAddress(label string) (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	pub, err := w.keys.pubAt(branchReceive, w.nextRecv)
	if err != nil {
		return "", err
	}
	w.nextRecv++
	if err := w.extendLocked(); err != nil {
		return "", err
	}
	a := w.encode(pub)
	if label != "" {
		w.labels[a] = label
	}
	return a, w.saveLocked()
}

// AddressInfo is a wallet address for listing.
type AddressInfo struct {
	Address  string
	Label    string
	Index    uint32
	Imported bool
}

// Addresses returns the issued receiving addresses and the imported keys.
func (w *Wallet) Addresses() []AddressInfo {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []AddressInfo
	for i := uint32(0); i < w.nextRecv; i++ {
		pub, err := w.keys.pubAt(branchReceive, i)
		if err != nil {
			continue
		}
		a := w.encode(pub)
		out = append(out, AddressInfo{Address: a, Label: w.labels[a], Index: i})
	}
	var imp []AddressInfo
	for pub := range w.imported {
		a := w.encode(pub)
		imp = append(imp, AddressInfo{Address: a, Label: w.labels[a], Imported: true})
	}
	sort.Slice(imp, func(i, j int) bool { return imp[i].Address < imp[j].Address })
	return append(out, imp...)
}

// IsMine says whether an address belongs to the wallet (issued, change or imported).
func (w *Wallet) IsMine(addr string) bool {
	pub, err := w.DecodeAddress(addr)
	if err != nil {
		return false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	_, ok := w.keys.secrets[pub]
	return ok
}

// Label returns an address's label.
func (w *Wallet) Label(addr string) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.labels[addr]
}

// SetLabel sets an address's label.
func (w *Wallet) SetLabel(addr, label string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if label == "" {
		delete(w.labels, addr)
	} else {
		w.labels[addr] = label
	}
	return w.saveLocked()
}

// DumpPrivKey returns an address's private key in the powmsec1… format.
func (w *Wallet) DumpPrivKey(addr string) (string, error) {
	pub, err := w.DecodeAddress(addr)
	if err != nil {
		return "", err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	sk, ok := w.keys.secrets[pub]
	if !ok {
		return "", ErrUnknownKey
	}
	return address.EncodeSecret(w.params.Bech32HRP, sk)
}

// ImportPrivKey adds a key (powmsec1…). The coins already on it are only seen by the wallet after a
// scan from `birth` (Rescan) — the caller decides about that. It returns the address and whether a
// rescan is needed.
//
// An imported key is NOT covered by the 24-word backup.
func (w *Wallet) ImportPrivKey(secret, label string, birth uint64) (string, bool, error) {
	sk, err := address.DecodeSecret(w.params.Bech32HRP, secret)
	if err != nil {
		return "", false, err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	pub, err := w.keys.addImported(sk)
	if err != nil {
		return "", false, err
	}
	a := w.encode(pub)
	if label != "" {
		w.labels[a] = label
	}
	if ref := w.keys.derived[pub]; ref.kind != kindImported {
		return a, false, w.saveLocked() // a derived key — the wallet already tracks it
	}
	_, had := w.imported[pub]
	w.imported[pub] = imported{Key: *sk, Birth: birth}
	needRescan := !had && w.synced && birth <= w.height
	return a, needRescan, w.saveLocked()
}

// ------------------------------------------------------------------ the file's content

type payloadJSON struct {
	Version    int               `json:"version"`
	Network    string            `json:"network"`
	Entropy    []byte            `json:"entropy"`
	Birth      uint64            `json:"birth"`
	NextRecv   uint32            `json:"next_receive"`
	NextChange uint32            `json:"next_change"`
	Imported   []importedJSON    `json:"imported,omitempty"`
	Labels     map[string]string `json:"labels,omitempty"`
	Synced     bool              `json:"synced"`
	Height     uint64            `json:"height"`
	Tip        []byte            `json:"tip,omitempty"`
	Recent     []blockRefJSON    `json:"recent,omitempty"`
	Coins      []coinJSON        `json:"coins,omitempty"`
	Txs        []txJSON          `json:"txs,omitempty"`
}

type importedJSON struct {
	Key   []byte `json:"key"`
	Birth uint64 `json:"birth"`
}

type blockRefJSON struct {
	H  uint64 `json:"h"`
	ID []byte `json:"id"`
}

type coinJSON struct {
	TxID        []byte `json:"txid"`
	Index       uint32 `json:"n"`
	Value       int64  `json:"value"`
	PubKey      []byte `json:"pub"`
	Height      uint64 `json:"height"`
	Coinbase    bool   `json:"coinbase,omitempty"`
	SpentBy     []byte `json:"spent_by,omitempty"`
	SpentHeight uint64 `json:"spent_height,omitempty"`
	PendingBy   []byte `json:"pending_by,omitempty"`
}

type txJSON struct {
	ID       []byte           `json:"id"`
	Height   uint64           `json:"height"`
	BlockID  []byte           `json:"block,omitempty"`
	Time     int64            `json:"time"`
	Received int64            `json:"received"`
	Spent    int64            `json:"spent"`
	Fee      int64            `json:"fee"`
	Coinbase bool             `json:"coinbase,omitempty"`
	Outputs  []Output         `json:"outputs"`
	Raw      []byte           `json:"raw,omitempty"`
	Inputs   []chain.Outpoint `json:"inputs,omitempty"`
}

func b32(b [32]byte) []byte {
	if b == [32]byte{} {
		return nil
	}
	return append([]byte{}, b[:]...)
}

func a32(b []byte) [32]byte {
	var out [32]byte
	copy(out[:], b)
	return out
}

func (w *Wallet) marshalPayload() ([]byte, error) {
	p := payloadJSON{Version: payloadVersion, Network: w.params.Name, Entropy: w.entropy,
		Birth: w.birth, NextRecv: w.nextRecv, NextChange: w.nextChange, Labels: w.labels,
		Synced: w.synced, Height: w.height, Tip: b32(w.tip)}
	for _, im := range w.imported {
		p.Imported = append(p.Imported, importedJSON{Key: im.Key[:], Birth: im.Birth})
	}
	for _, r := range w.recent {
		p.Recent = append(p.Recent, blockRefJSON{H: r.Height, ID: r.ID[:]})
	}
	for _, c := range w.coins {
		p.Coins = append(p.Coins, coinJSON{TxID: c.Outpoint.TxID[:], Index: c.Outpoint.Index,
			Value: c.Value, PubKey: c.PubKey[:], Height: c.Height, Coinbase: c.Coinbase,
			SpentBy: b32(c.SpentBy), SpentHeight: c.SpentHeight, PendingBy: b32(c.PendingBy)})
	}
	for _, t := range w.txs {
		p.Txs = append(p.Txs, txJSON{ID: t.ID[:], Height: t.Height, BlockID: b32(t.BlockID),
			Time: t.Time, Received: t.Received, Spent: t.Spent, Fee: t.Fee, Coinbase: t.Coinbase,
			Outputs: t.Outputs, Raw: t.Raw, Inputs: t.Inputs})
	}
	return json.Marshal(p)
}

func (w *Wallet) loadPayload(plain []byte) error {
	var p payloadJSON
	if err := json.Unmarshal(plain, &p); err != nil {
		return fmt.Errorf("wallet: cannot read wallet contents: %w", err)
	}
	if p.Version != payloadVersion {
		return ErrFileVersion
	}
	if p.Network != w.params.Name {
		return fmt.Errorf("%w (%s)", ErrWrongNetwork, p.Network)
	}
	w.birth, w.nextRecv, w.nextChange = p.Birth, p.NextRecv, p.NextChange
	if err := w.initKeys(p.Entropy); err != nil {
		return err
	}
	for _, im := range p.Imported {
		k := a32(im.Key)
		pub, err := w.keys.addImported(&k)
		if err != nil {
			return err
		}
		w.imported[pub] = imported{Key: k, Birth: im.Birth}
	}
	if p.Labels != nil {
		w.labels = p.Labels
	}
	w.synced, w.height, w.tip = p.Synced, p.Height, a32(p.Tip)
	for _, r := range p.Recent {
		w.recent = append(w.recent, blockRef{Height: r.H, ID: a32(r.ID)})
	}
	for _, c := range p.Coins {
		coin := &Coin{Outpoint: chain.Outpoint{TxID: a32(c.TxID), Index: c.Index}, Value: c.Value,
			PubKey: a32(c.PubKey), Height: c.Height, Coinbase: c.Coinbase, SpentBy: a32(c.SpentBy),
			SpentHeight: c.SpentHeight, PendingBy: a32(c.PendingBy)}
		w.coins[coin.Outpoint] = coin
	}
	for _, t := range p.Txs {
		w.txs[a32(t.ID)] = &Tx{ID: a32(t.ID), Height: t.Height, BlockID: a32(t.BlockID), Time: t.Time,
			Received: t.Received, Spent: t.Spent, Fee: t.Fee, Coinbase: t.Coinbase, Outputs: t.Outputs, Raw: t.Raw,
			Inputs: t.Inputs}
	}
	return nil
}
