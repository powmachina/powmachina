// Package finkey is a finality key on disk: the Ed25519 key a miner signs finality checkpoints with
// (the finality spec) and the record of the last block it signed. The node holds one in its datadir;
// a miner on somebody else's stratum holds its own (the stratum spec, `attest`), so that the vote of
// its work stays with it and not with whoever runs the stratum.
package finkey

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"sync"

	"powmachina/internal/chain"
)

// Signer is a finality key and the record of the last block it signed.
type Signer struct {
	mu   sync.Mutex
	priv ed25519.PrivateKey
	// Pub is the public key, the one that goes into the share commitment.
	Pub [32]byte
	// path is where the last signed height is recorded; empty for a key in memory.
	path       string
	lastHeight uint64
	lastID     [32]byte
	signed     bool
}

// New makes a signer of a key; recordPath "" keeps the record in memory only.
func New(priv ed25519.PrivateKey, recordPath string) *Signer {
	s := &Signer{priv: priv, path: recordPath}
	copy(s.Pub[:], priv.Public().(ed25519.PublicKey))
	return s
}

// Priv is the private key (for tests that need to sign by hand).
func (s *Signer) Priv() ed25519.PrivateKey { return s.priv }

// Load opens the key at keyPath, making one the first time (created reports that), and reads what
// it signed last from recordPath.
func Load(keyPath, recordPath string) (s *Signer, created bool, err error) {
	raw, err := os.ReadFile(keyPath)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		seed := make([]byte, ed25519.SeedSize)
		if _, err := rand.Read(seed); err != nil {
			return nil, false, err
		}
		if err := WriteFileSync(keyPath, []byte(hex.EncodeToString(seed)+"\n"), 0o600); err != nil {
			return nil, false, fmt.Errorf("the finality key cannot be written: %w", err)
		}
		raw, created = []byte(hex.EncodeToString(seed)), true
	case err != nil:
		return nil, false, fmt.Errorf("the finality key cannot be read: %w", err)
	}
	seed, err := hex.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, false, fmt.Errorf("%s is not a finality key (64 hex characters)", keyPath)
	}
	s = New(ed25519.NewKeyFromSeed(seed), recordPath)

	// What it signed last. Without this a restart after a deep reorg could sign a second block at
	// a height it already signed — the one thing a key must never do.
	if raw, err := os.ReadFile(recordPath); err == nil {
		var rec struct {
			Height uint64 `json:"height"`
			Block  string `json:"block"`
		}
		if err := json.Unmarshal(raw, &rec); err != nil {
			return nil, false, fmt.Errorf("%s: %w", recordPath, err)
		}
		id, err := hex.DecodeString(rec.Block)
		if err != nil || len(id) != 32 {
			return nil, false, fmt.Errorf("%s: the block is not an id", recordPath)
		}
		s.lastHeight, s.signed = rec.Height, true
		copy(s.lastID[:], id)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, false, err
	}
	return s, created, nil
}

// ErrRefused: the key has signed another block at that height or anything above it.
var ErrRefused = errors.New("finality: the key already signed another block at that height or above")

// Sign signs a block unless the key has signed another block at that height or anything above it.
//
// The record goes to disk before the signature exists: after a crash an attestation is missing,
// never a second one made. The same block again is signed again — Ed25519 is deterministic, so it
// is the same signature, and it is how finality catches up after the whole network restarted.
func (s *Signer) Sign(height uint64, id [32]byte) (chain.Attestation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.signed && (height < s.lastHeight || height == s.lastHeight && id != s.lastID) {
		return chain.Attestation{}, ErrRefused
	}
	if !s.signed || height != s.lastHeight {
		if s.path != "" {
			rec, _ := json.Marshal(map[string]any{"height": height, "block": hex.EncodeToString(id[:])})
			if err := WriteFileSync(s.path, rec, 0o600); err != nil {
				return chain.Attestation{}, fmt.Errorf("finality: the signed height cannot be recorded, not signing: %w", err)
			}
		}
		s.lastHeight, s.lastID, s.signed = height, id, true
	}
	return chain.Sign(s.priv, height, id), nil
}

// WriteFileSync writes a file whole or not at all: a temporary file, fsync, rename.
func WriteFileSync(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
