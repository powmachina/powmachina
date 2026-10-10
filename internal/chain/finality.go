package chain

import (
	"crypto/ed25519"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
	"sync"
)

// The finality layer: a quorum of signers finalizes the block at depth FinalityDepth. What is
// finalized is never rewritten by a reorg — regardless of work.
//
// The design is deliberately asymmetric:
//
//	the quorum CAN     finalize a block that PoW already built and our node verified
//	the quorum CANNOT  produce a block, rewrite history or stop the chain
//
// When the quorum is silent or contradicts itself, finality simply does not advance and the chain
// keeps running under the plain PoW rules (MESS). An exchange's safety is therefore not bought at
// the risk of stopping the network.

const (
	// FinalityDepth is the depth at which a block may be finalized (20 blocks ≈ 40 min).
	FinalityDepth uint64 = 20
	// FinalityThreshold is the number of signatures needed to finalize.
	FinalityThreshold = 5
	// FinalitySigners is the size of the quorum.
	FinalitySigners = 9
)

var finalityDomain = []byte("PowMachina/finality/v0")

var (
	ErrUnknownSigner  = errors.New("chain: the signer is not in the quorum")
	ErrBadSignature   = errors.New("chain: invalid attestation signature")
	ErrSignerConflict = errors.New("chain: the signer attested two different blocks at the same height")
	// ErrDuplicateAttestation is an attestation we already have. It is not a fault — it is a
	// signal to the caller **not to relay it**, otherwise gossip never ends.
	ErrDuplicateAttestation = errors.New("chain: we already have the attestation")
	ErrFinalityRewrite      = errors.New("chain: the reorg would rewrite a finalized block")
)

// SignerSet is the quorum valid from a given height. The keys are in the binary; they are rotated by releasing a new version.
type SignerSet struct {
	ActiveFrom uint64
	Keys       []ed25519.PublicKey
	Threshold  int
}

// Attestation is one quorum member's signature over a particular block.
type Attestation struct {
	Height    uint64
	BlockID   [32]byte
	SignerIdx uint8
	Signature []byte
}

// AttestationMessage is what gets signed. The domain prevents the signature from being carried elsewhere.
func AttestationMessage(height uint64, blockID [32]byte) []byte {
	msg := make([]byte, 0, len(finalityDomain)+8+32)
	msg = append(msg, finalityDomain...)
	var h [8]byte
	binary.LittleEndian.PutUint64(h[:], height)
	msg = append(msg, h[:]...)
	msg = append(msg, blockID[:]...)
	return msg
}

// Verify checks the signature against the quorum.
func (a *Attestation) Verify(set *SignerSet) error {
	if set == nil || int(a.SignerIdx) >= len(set.Keys) {
		return ErrUnknownSigner
	}
	if !ed25519.Verify(set.Keys[a.SignerIdx], AttestationMessage(a.Height, a.BlockID), a.Signature) {
		return ErrBadSignature
	}
	return nil
}

// Equivocation is proof that a quorum member signed two different blocks at the same height.
// The node gossips it on and turns finality off for that height — it does not stop, it just falls back to PoW.
type Equivocation struct {
	SignerIdx uint8
	Height    uint64
	First     Attestation
	Second    Attestation
}

func (e *Equivocation) Error() string {
	return fmt.Sprintf("signer %d attested two blocks at height %d (%x… and %x…)",
		e.SignerIdx, e.Height, e.First.BlockID[:4], e.Second.BlockID[:4])
}

// ChainLookup returns the ID of the block the node has at a given height on its own active chain.
// Without it the quorum could "finalize" a block we have never seen or verified.
type ChainLookup func(height uint64) ([32]byte, bool)

// attKey is exactly one attestation: the height, the signer and the block. It serves as "I have
// seen this" — without it gossip never stops (see Add).
type attKey struct {
	height  uint64
	signer  uint8
	blockID [32]byte
}

// FinalityState collects attestations and computes the height the chain is definitive up to.
//
// It is **safe for concurrent use**: attestations arrive from every peer's read loop and from RPC
// (the standalone signer), so several goroutines push into the state at once. Without a lock that
// ends as a crash on the map (`assignment to entry in nil map`), and that is the better case — a
// racy map can also silently forget an attestation.
//
// Mind the lock order: `CheckReorg` is called **under the header chain's lock**, so `FinalityState`
// must not hold its own lock while it asks the chain. That is why `Update` first picks the
// candidates, releases the lock, looks into the chain and only then decides.
type FinalityState struct {
	mu        sync.RWMutex
	set       *SignerSet
	byHeight  map[uint64]map[uint8]Attestation
	seen      map[attKey]bool
	disputed  map[uint64]bool
	finalized uint64
	finalID   [32]byte
	hasFinal  bool
}

func NewFinalityState(set *SignerSet) *FinalityState {
	return &FinalityState{
		set:      set,
		byHeight: make(map[uint64]map[uint8]Attestation),
		seen:     make(map[attKey]bool),
		disputed: make(map[uint64]bool),
	}
}

// Add files an attestation. It returns proof of double signing if it uncovers one.
//
// **An attestation we have already seen returns ErrDuplicateAttestation** — and that matters more
// than it looks: the node only gossips on what Add accepted without an error. If a duplicate did
// not return an error, every node would forward every attestation every time, so gossip between
// three or more nodes would loop forever and flood the peers ("sending too many messages", a ban
// for flooding).
func (f *FinalityState) Add(a Attestation) (*Equivocation, error) {
	if err := a.Verify(f.set); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	key := attKey{height: a.Height, signer: a.SignerIdx, blockID: a.BlockID}
	if f.seen[key] {
		return nil, ErrDuplicateAttestation
	}
	f.seen[key] = true

	at, ok := f.byHeight[a.Height]
	if !ok {
		at = make(map[uint8]Attestation)
		f.byHeight[a.Height] = at
	}
	if prev, had := at[a.SignerIdx]; had {
		// The same signer, the same height, a different block — proof of double signing.
		// It is produced once for every **new** such attestation; repeats pass above as a
		// duplicate and are not relayed.
		f.disputed[a.Height] = true
		return &Equivocation{SignerIdx: a.SignerIdx, Height: a.Height, First: prev, Second: a}, ErrSignerConflict
	}
	at[a.SignerIdx] = a
	return nil, nil
}

// pruneBelow drops the attestations below a given height. Finalized heights are never recomputed,
// so there is no need to keep them — and without this the state would grow with the chain.
func (f *FinalityState) pruneBelow(height uint64) {
	for h := range f.byHeight {
		if h < height {
			delete(f.byHeight, h)
			delete(f.disputed, h)
		}
	}
	for k := range f.seen {
		if k.height < height {
			delete(f.seen, k)
		}
	}
}

// Update recomputes the finalized height. Only a block that:
//   - is at least FinalityDepth below the tip,
//   - has at least Threshold signatures from different quorum members,
//   - and **is on our own chain** (otherwise we ignore the attestation),
//
// can be finalized.
func (f *FinalityState) Update(tipHeight uint64, lookup ChainLookup) {
	if tipHeight < FinalityDepth {
		return
	}
	maxHeight := tipHeight - FinalityDepth

	// 1. the candidates under our own lock
	f.mu.Lock()
	heights := make([]uint64, 0, len(f.byHeight))
	for h := range f.byHeight {
		if h <= maxHeight && h > f.finalized && !f.disputed[h] {
			heights = append(heights, h)
		}
	}
	f.mu.Unlock()
	if len(heights) == 0 {
		return
	}
	sort.Slice(heights, func(i, j int) bool { return heights[i] > heights[j] })

	// 2. the questions to the chain **without our lock** — lookup takes the header chain's lock
	//    and the caller of CheckReorg holds that same lock. Holding both here would be a cycle.
	ours := make(map[uint64][32]byte, len(heights))
	for _, h := range heights {
		if id, ok := lookup(h); ok {
			ours[h] = id
		}
	}

	// 3. the decision under our lock, recomputed (the state may have changed in the meantime)
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, h := range heights {
		id, ok := ours[h]
		if !ok || h <= f.finalized || f.disputed[h] {
			continue
		}
		count := 0
		for _, a := range f.byHeight[h] {
			if a.BlockID == id {
				count++
			}
		}
		if count >= f.set.Threshold {
			f.finalized = h
			f.finalID = id
			f.hasFinal = true
			f.pruneBelow(h)
			return // heights are sorted descending, so this is the highest finalized height
		}
	}
}

// InRange returns the attestations for heights `from` … `to` (inclusive), at most `max` of them.
// It serves the answer to a peer that asks after a restart what it is missing.
func (f *FinalityState) InRange(from, to uint64, max int) []Attestation {
	if max <= 0 || to < from {
		return nil
	}
	f.mu.RLock()
	defer f.mu.RUnlock()
	out := make([]Attestation, 0, max)
	for h := from; h <= to; h++ {
		for _, a := range f.byHeight[h] {
			if len(out) >= max {
				return out
			}
			out = append(out, a)
		}
	}
	return out
}

// Finalized returns the highest finalized height.
func (f *FinalityState) Finalized() (uint64, [32]byte, bool) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.finalized, f.finalID, f.hasFinal
}

// CheckReorg verifies that a proposed reorg does not reach below a finalized block.
// forkHeight is the height of the common ancestor — the reorg changes everything above it.
func (f *FinalityState) CheckReorg(forkHeight uint64) error {
	f.mu.RLock()
	defer f.mu.RUnlock()
	if !f.hasFinal {
		return nil
	}
	if forkHeight < f.finalized {
		return ErrFinalityRewrite
	}
	return nil
}

// Sign creates an attestation. Only a quorum member uses it, in their own tool, not the node.
func Sign(priv ed25519.PrivateKey, idx uint8, height uint64, blockID [32]byte) Attestation {
	return Attestation{
		Height:    height,
		BlockID:   blockID,
		SignerIdx: idx,
		Signature: ed25519.Sign(priv, AttestationMessage(height, blockID)),
	}
}
