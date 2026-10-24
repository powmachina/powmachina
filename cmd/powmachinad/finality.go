package main

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"

	"powmachina/internal/chain"
	"powmachina/internal/finkey"
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

// loadFinalitySigner opens the node's key in the datadir, making one the first time.
func loadFinalitySigner(datadir string) (*finkey.Signer, error) {
	keyPath := filepath.Join(datadir, finalityKeyFile)
	s, created, err := finkey.Load(keyPath, filepath.Join(datadir, finalitySignedFile))
	if err != nil {
		return nil, err
	}
	if created {
		log.Printf("finality: a new key in %s", keyPath)
	}
	return s, nil
}

// writeFileSync writes a file whole or not at all: a temporary file, fsync, rename.
func writeFileSync(path string, data []byte, perm os.FileMode) error {
	return finkey.WriteFileSync(path, data, perm)
}

// FinalityKey is the node's finality key, the one that goes into the coinbase (nil = none).
func (n *Node) FinalityKey() *[32]byte {
	if n.finality == nil || n.signer == nil {
		return nil
	}
	k := n.signer.Pub
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
	_ = n.addAttestation(att, from)
	return nil // not a reason to ban: the peer may be on another branch or ahead of us
}

// addAttestation is acceptAttestation that says why an attestation was not taken — for a miner
// signing through the stratum port, who is told.
func (n *Node) addAttestation(att chain.Attestation, from string) error {
	if n.finality == nil {
		return errors.New("the network does not use finality")
	}
	ev, err := n.finality.Add(att, n.quorumAt(att.Height))
	if ev != nil {
		log.Printf("PROOF OF DOUBLE SIGNING: %v", ev)
		n.saveFinality()
		if payload, encErr := p2p.EncodeEquivocation(ev); encErr == nil {
			n.announceRaw(p2p.CmdEquivocation, payload, "")
		}
		return errors.New("the key signed two blocks at that height")
	}
	if err != nil {
		return err
	}
	n.updateFinality()
	if payload, err := p2p.EncodeAttestation(att); err == nil {
		n.announceRaw(p2p.CmdAttestation, payload, from)
	}
	return nil
}

// AttestDue implements stratum.Backend: the block at the finality depth below the tip, when the
// key has a seat in its quorum — what a miner with its own key should sign now.
func (n *Node) AttestDue(key [32]byte) (uint64, [32]byte, bool) {
	if n.finality == nil {
		return 0, [32]byte{}, false
	}
	_, tip, _ := n.chain.Tip()
	if tip < chain.FinalityDepth {
		return 0, [32]byte{}, false
	}
	target := tip - chain.FinalityDepth
	id, ok := n.chain.BlockIDAt(target)
	if !ok || n.quorumAt(target).Weight(key) == 0 {
		return 0, [32]byte{}, false
	}
	return target, id, true
}

// SubmitAttestation implements stratum.Backend: a miner's signature from the stratum port.
// One the node already has — or a height already final — is not the miner's mistake.
//
// It has to be the block of that height on the node's chain: the port asks for those and takes
// nothing else, so it is not a way to put a signature of another branch into the network.
func (n *Node) SubmitAttestation(a chain.Attestation) error {
	if id, ok := n.chain.BlockIDAt(a.Height); !ok || id != a.BlockID {
		n.stats.inc(&n.stats.attests, "refused")
		return errors.New("not the block of that height on this node's chain")
	}
	if err := n.addAttestation(a, ""); err != nil && !errors.Is(err, chain.ErrDuplicateAttestation) {
		n.stats.inc(&n.stats.attests, "refused")
		return err
	}
	n.stats.inc(&n.stats.attests, "accepted")
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
	if n.quorumAt(target).Weight(n.signer.Pub) == 0 {
		return
	}
	att, err := n.signer.Sign(target, id)
	if err != nil {
		if !errors.Is(err, finkey.ErrRefused) {
			log.Printf("⚠ %v", err)
		}
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
