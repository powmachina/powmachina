package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"powmachina/internal/chain"
	"powmachina/internal/p2p"
)

// Finality on the node's side (the finality spec).
//
// Every node has a finality key. It goes into the coinbase of every block built from the node's
// templates — its own -mine, its stratum port, a pool on its RPC — and every block mined with it is
// a seat in the quorum for the day that follows. The node signs the block at the finality depth by
// itself whenever it holds a seat; nobody has to run anything else, and nobody has to hand a key
// to anybody.

const (
	finalityKeyFile    = "finality.key"
	finalitySignedFile = "finality.signed"
	finalityStateFile  = "finality.json"
)

// finalitySigner is the node's key and the record of the last block it signed.
type finalitySigner struct {
	mu   sync.Mutex
	priv ed25519.PrivateKey
	pub  [32]byte
	// path is where the last signed height is recorded; empty for a node in memory.
	path       string
	lastHeight uint64
	lastID     [32]byte
	signed     bool
}

func newFinalitySigner(priv ed25519.PrivateKey, path string) *finalitySigner {
	s := &finalitySigner{priv: priv, path: path}
	copy(s.pub[:], priv.Public().(ed25519.PublicKey))
	return s
}

// loadFinalitySigner opens the node's key in the datadir, making one the first time.
func loadFinalitySigner(datadir string) (*finalitySigner, error) {
	keyPath := filepath.Join(datadir, finalityKeyFile)
	raw, err := os.ReadFile(keyPath)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		seed := make([]byte, ed25519.SeedSize)
		if _, err := rand.Read(seed); err != nil {
			return nil, err
		}
		if err := writeFileSync(keyPath, []byte(hex.EncodeToString(seed)+"\n"), 0o600); err != nil {
			return nil, fmt.Errorf("the finality key cannot be written: %w", err)
		}
		raw = []byte(hex.EncodeToString(seed))
		log.Printf("finality: a new key in %s", keyPath)
	case err != nil:
		return nil, fmt.Errorf("the finality key cannot be read: %w", err)
	}
	seed, err := hex.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("%s is not a finality key (64 hex characters)", keyPath)
	}
	s := newFinalitySigner(ed25519.NewKeyFromSeed(seed), filepath.Join(datadir, finalitySignedFile))

	// What it signed last. Without this a restart after a deep reorg could sign a second block at
	// a height it already signed — the one thing a key must never do.
	if raw, err := os.ReadFile(s.path); err == nil {
		var rec struct {
			Height uint64 `json:"height"`
			Block  string `json:"block"`
		}
		if err := json.Unmarshal(raw, &rec); err != nil {
			return nil, fmt.Errorf("%s: %w", s.path, err)
		}
		id, err := hex.DecodeString(rec.Block)
		if err != nil || len(id) != 32 {
			return nil, fmt.Errorf("%s: the block is not an id", s.path)
		}
		s.lastHeight, s.signed = rec.Height, true
		copy(s.lastID[:], id)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	return s, nil
}

// sign signs a block unless the key has signed another block at that height or anything above it.
// The record goes to disk before the signature exists: after a crash an attestation is missing,
// never a second one made. The same block again is signed again — Ed25519 is deterministic, so it
// is the same signature, and it is how finality catches up after the whole network restarted.
func (s *finalitySigner) sign(height uint64, id [32]byte) (chain.Attestation, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.signed && (height < s.lastHeight || height == s.lastHeight && id != s.lastID) {
		return chain.Attestation{}, false
	}
	if !s.signed || height != s.lastHeight {
		if s.path != "" {
			rec, _ := json.Marshal(map[string]any{"height": height, "block": hex.EncodeToString(id[:])})
			if err := writeFileSync(s.path, rec, 0o600); err != nil {
				log.Printf("⚠ finality: the signed height cannot be recorded, not signing: %v", err)
				return chain.Attestation{}, false
			}
		}
		s.lastHeight, s.lastID, s.signed = height, id, true
	}
	return chain.Sign(s.priv, height, id), true
}

// writeFileSync writes a file whole or not at all: a temporary file, fsync, rename.
func writeFileSync(path string, data []byte, perm os.FileMode) error {
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

// FinalityKey is the node's finality key, the one that goes into the coinbase (nil = none).
func (n *Node) FinalityKey() *[32]byte {
	if n.finality == nil || n.signer == nil {
		return nil
	}
	k := n.signer.pub
	return &k
}

// seatAt reads the seat of the block at a height of the active chain: its lane from the header,
// its key from the coinbase. A block's seat never changes, so it is cached by id.
func (n *Node) seatAt(height uint64) (chain.Seat, bool) {
	id, ok := n.chain.BlockIDAt(height)
	if !ok {
		return chain.Seat{}, false
	}
	n.seatMu.Lock()
	s, ok := n.seats[id]
	n.seatMu.Unlock()
	if ok {
		return s, true
	}
	h, ok := n.chain.HeaderByID(id)
	if !ok {
		return chain.Seat{}, false
	}
	blk, err := n.store.Block(id)
	if err != nil || len(blk.Txs) == 0 {
		return chain.Seat{}, false
	}
	s = chain.Seat{Lane: h.Lane}
	s.Key, s.HasKey = blk.FinalityKey()
	n.seatMu.Lock()
	// A few windows' worth is plenty; a reorg or a long run just starts the cache over.
	if len(n.seats) > 4*int(n.params.FinalityWindow)+64 {
		clear(n.seats)
		clear(n.quorums)
	}
	n.seats[id] = s
	n.seatMu.Unlock()
	return s, true
}

// quorumAt is the quorum for the block at a height of the active chain, nil while it cannot be
// computed. It depends only on the blocks below that block, so it is cached by the block's id.
func (n *Node) quorumAt(height uint64) *chain.Quorum {
	if n.finality == nil {
		return nil
	}
	id, ok := n.chain.BlockIDAt(height)
	if !ok {
		return nil
	}
	n.seatMu.Lock()
	q, ok := n.quorums[id]
	n.seatMu.Unlock()
	if ok {
		return q
	}
	q = chain.QuorumFor(height, n.params.FinalityWindow, n.seatAt)
	if q == nil {
		return nil
	}
	n.seatMu.Lock()
	n.quorums[id] = q
	n.seatMu.Unlock()
	return q
}

// acceptAttestation verifies a finality attestation, files it and relays it on.
//
// The key has to have a seat in the quorum of that height on our chain — otherwise the attestation
// is dropped and not relayed, so nobody can flood the network with made-up keys. A node that is
// behind asks for what it missed later (pollAttestations).
func (n *Node) acceptAttestation(att chain.Attestation, from string) error {
	if n.finality == nil {
		return nil // the network does not use finality
	}
	ev, err := n.finality.Add(att, n.quorumAt(att.Height))
	if ev != nil {
		log.Printf("PROOF OF DOUBLE SIGNING: %v", ev)
		n.saveFinality()
		if payload, encErr := p2p.EncodeEquivocation(ev); encErr == nil {
			n.announceRaw(p2p.CmdEquivocation, payload, "")
		}
		return nil
	}
	if err != nil {
		return nil // not a reason to ban: the peer may be on another branch or ahead of us
	}
	n.updateFinality()
	if payload, err := p2p.EncodeAttestation(att); err == nil {
		n.announceRaw(p2p.CmdAttestation, payload, from)
	}
	return nil
}

// acceptEquivocation files a proof of double signing from the network and passes it on once.
func (n *Node) acceptEquivocation(ev *chain.Equivocation, from string) {
	if n.finality == nil || !n.finality.AddEquivocation(ev) {
		return
	}
	log.Printf("PROOF OF DOUBLE SIGNING from the network: %v", ev)
	n.saveFinality()
	if payload, err := p2p.EncodeEquivocation(ev); err == nil {
		n.announceRaw(p2p.CmdEquivocation, payload, from)
	}
}

// updateFinality recomputes the finalized height and records it when it moved.
func (n *Node) updateFinality() {
	before, _, hadFinal := n.finality.Finalized()
	_, height, _ := n.chain.Tip()
	n.finality.Update(height, n.chain.BlockIDAt, n.quorumAt)
	if after, id, ok := n.finality.Finalized(); ok && (!hadFinal || after > before) {
		log.Printf("finality advanced to height %d (%x)", after, id[:6])
		n.saveFinality()
	}
}

// attestTip signs the block at the finality depth below a new tip if the node has a seat in its
// quorum. Only at the tip: a node catching up has nothing to say about blocks the network
// finalized long ago.
func (n *Node) attestTip(applied *chain.Block) {
	if n.finality == nil || n.signer == nil || n.noAttest || applied == nil {
		return
	}
	height := uint64(applied.Header.Height)
	if _, tip, _ := n.chain.Tip(); tip != height || height < chain.FinalityDepth {
		return
	}
	target := height - chain.FinalityDepth
	id, ok := n.chain.BlockIDAt(target)
	if !ok {
		return
	}
	if n.quorumAt(target).Weight(n.signer.pub) == 0 {
		return
	}
	att, ok := n.signer.sign(target, id)
	if !ok {
		return
	}
	_ = n.acceptAttestation(att, "")
}

// finalityRecord is what survives a restart: how far the chain is final and the keys caught signing twice.
type finalityRecord struct {
	Height uint64   `json:"height"`
	Block  string   `json:"block"`
	Caught []string `json:"caught,omitempty"` // proofs of double signing, hex of the wire form
}

// saveFinality records the finality state in the datadir. Before this the state lived only in
// memory, and a restart of the whole network lost it until the next blocks were signed.
func (n *Node) saveFinality() {
	if n.datadir == "" || n.finality == nil {
		return
	}
	height, id, ok := n.finality.Finalized()
	rec := finalityRecord{}
	if ok {
		rec.Height, rec.Block = height, hex.EncodeToString(id[:])
	}
	for _, ev := range n.finality.Caught() {
		if raw, err := p2p.EncodeEquivocation(ev); err == nil {
			rec.Caught = append(rec.Caught, hex.EncodeToString(raw))
		}
	}
	raw, err := json.Marshal(rec)
	if err != nil {
		return
	}
	n.finalityMu.Lock()
	defer n.finalityMu.Unlock()
	if err := writeFileSync(filepath.Join(n.datadir, finalityStateFile), raw, 0o644); err != nil {
		log.Printf("⚠ finality: the state cannot be recorded: %v", err)
	}
}

// loadFinality puts back what saveFinality recorded. A finalized block that is not on the chain
// the node has is not taken back — the datadir changed under it, and finality will come again.
func (n *Node) loadFinality() {
	if n.datadir == "" || n.finality == nil {
		return
	}
	raw, err := os.ReadFile(filepath.Join(n.datadir, finalityStateFile))
	if err != nil {
		return
	}
	var rec finalityRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		log.Printf("⚠ finality: %s cannot be read: %v", finalityStateFile, err)
		return
	}
	for _, h := range rec.Caught {
		if b, err := hex.DecodeString(h); err == nil {
			if ev, err := p2p.DecodeEquivocation(b); err == nil {
				n.finality.AddEquivocation(ev)
			}
		}
	}
	id, err := hex.DecodeString(rec.Block)
	if err != nil || len(id) != 32 {
		return
	}
	var want [32]byte
	copy(want[:], id)
	if have, ok := n.chain.BlockIDAt(rec.Height); ok && have == want {
		n.finality.Restore(rec.Height, want)
		log.Printf("finality: final up to height %d (from the datadir)", rec.Height)
	}
}
