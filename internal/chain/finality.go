package chain

import (
	"crypto/ed25519"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"sort"
	"sync"

	"powmachina/internal/mh"
)

// The finality layer: the miners of the recent blocks finalize the block at depth FinalityDepth.
// What is finalized is never rewritten by a reorg — regardless of work.
//
// Nobody appoints the quorum. A miner's node puts its finality key into the share commitment of
// the blocks it builds (Block.FinalityKey), under their PoW; the quorum for a height is every key in the FinalityWindow blocks
// below it, each weighing as many blocks as it mined there, and each lane holding half of the
// weight — so the quorum is earned the same way the blocks are, on the CPU and the GPU alike.
// A block is final once keys holding more than two thirds of that weight have signed it.
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
	// FinalityDepth is the depth at which a block may be finalized (80 blocks ≈ 40 min).
	FinalityDepth uint64 = 80
	// FinalityWindow is how many blocks below a height make up its quorum on mainnet and testnet
	// (2 880 blocks ≈ a day).
	// Renting hashrate for an hour buys a sliver of it; holding a third of it takes a day's worth
	// of blocks on the public chain. Chosen with cmd/powm-finsim.
	FinalityWindow uint64 = 2_880
	// RegtestFinalityWindow is the window on regtest, short so that a test reaches finality.
	RegtestFinalityWindow uint64 = 10
	// A block is final when the keys that signed it hold more than FinalityNum/FinalityDen of the
	// quorum's weight. Two thirds: two conflicting blocks can only both be final if more than a
	// third of the weight signed both, and a key that does that is caught (Equivocation).
	FinalityNum = 2
	FinalityDen = 3
)

var finalityDomain = []byte("PowMachina/finality/v1")

var (
	ErrUnknownSigner  = errors.New("chain: the key has no seat in the quorum for that height")
	ErrBadSignature   = errors.New("chain: invalid attestation signature")
	ErrSignerConflict = errors.New("chain: the key attested two different blocks at the same height")
	// ErrDuplicateAttestation is an attestation we already have. It is not a fault — it is a
	// signal to the caller **not to relay it**, otherwise gossip never ends.
	ErrDuplicateAttestation = errors.New("chain: we already have the attestation")
	ErrFinalityRewrite      = errors.New("chain: the reorg would rewrite a finalized block")
)

// Seat is what the quorum needs to know about one block: its lane and its miner's key.
type Seat struct {
	Lane   mh.Lane
	Key    [32]byte
	HasKey bool
}

// SeatLookup returns the seat of the block at a height on the node's own active chain.
type SeatLookup func(height uint64) (Seat, bool)

// Quorum is who may finalize the block at one height, and with how much weight.
type Quorum struct {
	Height uint64
	weight map[[32]byte]uint64
	total  uint64
}

// QuorumFor computes the quorum for the block at `height` from the `window` blocks below it.
// Each lane holds half of the weight: in a window of nC blocks of lane C and nG of lane G a block
// of lane C weighs nG and one of lane G nC — whoever mines only one lane holds at most half,
// however much hashrate they bring. A block without a key weighs as much as any other and
// belongs to nobody, so it counts as a signature that never comes.
//
// There is no quorum (nil) until the window is full: in the chain's first day the first few miners
// would be the whole quorum, and finality held by whoever was there first is worse than none.
func QuorumFor(height, window uint64, seats SeatLookup) *Quorum {
	if window == 0 || height <= window {
		return nil
	}
	var count [len(mh.Lanes)]map[[32]byte]uint64
	var n [len(mh.Lanes)]uint64
	for i := range count {
		count[i] = make(map[[32]byte]uint64)
	}
	for h := height - window; h < height; h++ {
		s, ok := seats(h)
		if !ok || int(s.Lane) >= len(mh.Lanes) {
			return nil
		}
		n[s.Lane]++
		if !s.HasKey {
			continue
		}
		count[s.Lane][s.Key]++
	}
	// A lane without a block in the window leaves the whole weight to the other one.
	var mult [len(mh.Lanes)]uint64
	switch {
	case n[mh.LaneC] == 0:
		mult[mh.LaneG] = 1
	case n[mh.LaneG] == 0:
		mult[mh.LaneC] = 1
	default:
		mult[mh.LaneC], mult[mh.LaneG] = n[mh.LaneG], n[mh.LaneC]
	}
	q := &Quorum{Height: height, weight: make(map[[32]byte]uint64)}
	for lane := range count {
		for key, c := range count[lane] {
			q.weight[key] += c * mult[lane]
		}
		q.total += n[lane] * mult[lane]
	}
	return q
}

// Weight is the weight of one key in the quorum (0 = no seat).
func (q *Quorum) Weight(key [32]byte) uint64 {
	if q == nil {
		return 0
	}
	return q.weight[key]
}

// Total is the weight of the whole window, the keyless blocks included.
func (q *Quorum) Total() uint64 {
	if q == nil {
		return 0
	}
	return q.total
}

// Members is the number of keys with a seat.
func (q *Quorum) Members() int {
	if q == nil {
		return 0
	}
	return len(q.weight)
}

// Enough says whether keys holding `signed` of the weight finalize the block.
func (q *Quorum) Enough(signed uint64) bool {
	return q != nil && q.total > 0 && signed*FinalityDen > q.total*FinalityNum
}

// Attestation is one miner's signature over a particular block.
type Attestation struct {
	Height    uint64
	BlockID   [32]byte
	Key       [32]byte
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

// Verify checks the signature against the key the attestation names.
func (a *Attestation) Verify() error {
	if len(a.Signature) != ed25519.SignatureSize ||
		!ed25519.Verify(ed25519.PublicKey(a.Key[:]), AttestationMessage(a.Height, a.BlockID), a.Signature) {
		return ErrBadSignature
	}
	return nil
}

// Sign creates an attestation with a miner's finality key.
func Sign(priv ed25519.PrivateKey, height uint64, blockID [32]byte) Attestation {
	a := Attestation{Height: height, BlockID: blockID, Signature: ed25519.Sign(priv, AttestationMessage(height, blockID))}
	copy(a.Key[:], priv.Public().(ed25519.PublicKey))
	return a
}

// Equivocation is proof that a key signed two different blocks at the same height. It is
// self-contained — anybody can check both signatures — so it travels the network on its own.
type Equivocation struct {
	Key    [32]byte
	Height uint64
	First  Attestation
	Second Attestation
}

func (e *Equivocation) Error() string {
	return fmt.Sprintf("key %x… attested two blocks at height %d (%x… and %x…)",
		e.Key[:4], e.Height, e.First.BlockID[:4], e.Second.BlockID[:4])
}

// Verify checks that the proof is what it says: one key, one height, two blocks, two valid signatures.
func (e *Equivocation) Verify() error {
	if e.First.Key != e.Key || e.Second.Key != e.Key || e.First.Height != e.Height ||
		e.Second.Height != e.Height || e.First.BlockID == e.Second.BlockID {
		return ErrBadSignature
	}
	if err := e.First.Verify(); err != nil {
		return err
	}
	return e.Second.Verify()
}

// ChainLookup returns the ID of the block the node has at a given height on its own active chain.
// Without it the quorum could "finalize" a block we have never seen or verified.
type ChainLookup func(height uint64) ([32]byte, bool)

// QuorumLookup returns the quorum for the block at a height on the node's own active chain, nil
// when it cannot be computed (the window is not full or not all of it is here yet).
type QuorumLookup func(height uint64) *Quorum

// attKey is exactly one attestation: the height, the key and the block. It serves as "I have seen
// this" — without it gossip never stops (see Add).
type attKey struct {
	height  uint64
	key     [32]byte
	blockID [32]byte
}

// FinalityState collects attestations and computes the height the chain is definitive up to.
//
// It is **safe for concurrent use**: attestations arrive from every peer's read loop and from the
// node's own signer, so several goroutines push into the state at once.
//
// Mind the lock order: `CheckReorg` is called **under the header chain's lock**, so `FinalityState`
// must not hold its own lock while it asks the chain. The quorum of an attestation is therefore
// computed by the caller before Add, and `Update` first picks the candidates, releases the lock,
// looks into the chain and only then decides.
type FinalityState struct {
	mu        sync.RWMutex
	byHeight  map[uint64]map[[32]byte]Attestation
	seen      map[attKey]bool
	caught    map[[32]byte]*Equivocation // keys proven to have signed two blocks at one height
	finalized uint64
	finalID   [32]byte
	hasFinal  bool
}

func NewFinalityState() *FinalityState {
	return &FinalityState{
		byHeight: make(map[uint64]map[[32]byte]Attestation),
		seen:     make(map[attKey]bool),
		caught:   make(map[[32]byte]*Equivocation),
	}
}

// Add files an attestation whose key has a seat in `q`, the quorum of its height. It returns proof
// of double signing if it uncovers one.
//
// **An attestation we have already seen returns ErrDuplicateAttestation** — and that matters more
// than it looks: the node only gossips on what Add accepted without an error. If a duplicate did
// not return an error, every node would forward every attestation every time, so gossip between
// three or more nodes would loop forever and flood the peers.
func (f *FinalityState) Add(a Attestation, q *Quorum) (*Equivocation, error) {
	if q == nil || q.Height != a.Height || q.Weight(a.Key) == 0 {
		return nil, ErrUnknownSigner
	}
	if err := a.Verify(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	if a.Height <= f.finalized && f.hasFinal {
		return nil, ErrDuplicateAttestation // finalized heights are never recomputed
	}
	key := attKey{height: a.Height, key: a.Key, blockID: a.BlockID}
	if f.seen[key] {
		return nil, ErrDuplicateAttestation
	}
	f.seen[key] = true

	at, ok := f.byHeight[a.Height]
	if !ok {
		at = make(map[[32]byte]Attestation)
		f.byHeight[a.Height] = at
	}
	if prev, had := at[a.Key]; had {
		// The same key, the same height, a different block — proof of double signing. From now on
		// the key's signatures count for nothing, at any height; its weight stays in the quorum,
		// so it can only make finality harder, never easier.
		ev := &Equivocation{Key: a.Key, Height: a.Height, First: prev, Second: a}
		if _, known := f.caught[a.Key]; !known {
			f.caught[a.Key] = ev
		}
		return ev, ErrSignerConflict
	}
	at[a.Key] = a
	return nil, nil
}

// AddEquivocation files a proof of double signing that came from the network. `q` is the quorum
// of the proof's height: a key without a seat in it could not have moved finality, and anybody can
// make up keys that sign twice — kept and relayed, they would grow the node's memory, its finality
// file and the gossip without end. It returns false when the proof is not
// valid, its key has no seat, or the key is already known to have done it.
func (f *FinalityState) AddEquivocation(e *Equivocation, q *Quorum) bool {
	if q == nil || q.Height != e.Height || q.Weight(e.Key) == 0 {
		return false
	}
	if e.Verify() != nil {
		return false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, known := f.caught[e.Key]; known {
		return false
	}
	f.caught[e.Key] = e
	return true
}

// Caught returns the proofs of double signing the node knows of.
func (f *FinalityState) Caught() []*Equivocation {
	f.mu.RLock()
	defer f.mu.RUnlock()
	out := make([]*Equivocation, 0, len(f.caught))
	for _, e := range f.caught {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return string(out[i].Key[:]) < string(out[j].Key[:]) })
	return out
}

// pruneBelow drops the attestations below a given height. Finalized heights are never recomputed,
// so there is no need to keep them — and without this the state would grow with the chain.
func (f *FinalityState) pruneBelow(height uint64) {
	for h := range f.byHeight {
		if h < height {
			delete(f.byHeight, h)
		}
	}
	for k := range f.seen {
		if k.height < height {
			delete(f.seen, k)
		}
	}
}

// signedLocked is the weight of the keys that attested `id` at `height` and were not caught
// signing twice.
func (f *FinalityState) signedLocked(height uint64, id [32]byte, q *Quorum) uint64 {
	var signed uint64
	for key, a := range f.byHeight[height] {
		if a.BlockID != id {
			continue
		}
		if _, bad := f.caught[key]; bad {
			continue
		}
		signed += q.Weight(key)
	}
	return signed
}

// Update recomputes the finalized height. Only a block that:
//   - is at least FinalityDepth below the tip,
//   - is signed by keys holding more than two thirds of its quorum,
//   - and **is on our own chain** (otherwise we ignore the attestation),
//
// can be finalized.
func (f *FinalityState) Update(tipHeight uint64, lookup ChainLookup, quorum QuorumLookup) {
	if tipHeight < FinalityDepth {
		return
	}
	maxHeight := tipHeight - FinalityDepth

	// 1. the candidates under our own lock
	f.mu.Lock()
	heights := make([]uint64, 0, len(f.byHeight))
	for h := range f.byHeight {
		if h <= maxHeight && (!f.hasFinal || h > f.finalized) {
			heights = append(heights, h)
		}
	}
	f.mu.Unlock()
	if len(heights) == 0 {
		return
	}
	sort.Slice(heights, func(i, j int) bool { return heights[i] > heights[j] })

	// 2. the questions to the chain **without our lock** — the lookups take the header chain's
	//    lock and the caller of CheckReorg holds that same lock. Holding both here would be a cycle.
	type candidate struct {
		id [32]byte
		q  *Quorum
	}
	ours := make(map[uint64]candidate, len(heights))
	for _, h := range heights {
		id, ok := lookup(h)
		if !ok {
			continue
		}
		if q := quorum(h); q != nil {
			ours[h] = candidate{id: id, q: q}
		}
	}

	// 3. the decision under our lock, recomputed (the state may have changed in the meantime)
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, h := range heights {
		c, ok := ours[h]
		if !ok || (f.hasFinal && h <= f.finalized) {
			continue
		}
		if c.q.Enough(f.signedLocked(h, c.id, c.q)) {
			f.finalized = h
			f.finalID = c.id
			f.hasFinal = true
			f.pruneBelow(h)
			return // heights are sorted descending, so this is the highest finalized height
		}
	}
}

// Signed returns the weight that signed the block `id` at `height` — what the RPC shows while a
// block is on its way to being final.
func (f *FinalityState) Signed(height uint64, id [32]byte, q *Quorum) uint64 {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.signedLocked(height, id, q)
}

// InRange returns the attestations for heights `from` … `to` (inclusive), at most `max` of them, in
// order of height. It serves the answer to a peer that asks after a restart what it is missing.
//
// The range comes off the wire, so the work must not depend on how wide it is: it walks the heights the
// node holds attestations for, never the range itself.
func (f *FinalityState) InRange(from, to uint64, max int) []Attestation {
	if max <= 0 || to < from {
		return nil
	}
	f.mu.RLock()
	defer f.mu.RUnlock()
	heights := make([]uint64, 0, len(f.byHeight))
	for h := range f.byHeight {
		if h >= from && h <= to {
			heights = append(heights, h)
		}
	}
	slices.Sort(heights)
	out := make([]Attestation, 0, max)
	for _, h := range heights {
		keys := make([][32]byte, 0, len(f.byHeight[h]))
		for k := range f.byHeight[h] {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool { return string(keys[i][:]) < string(keys[j][:]) })
		for _, k := range keys {
			if len(out) >= max {
				return out
			}
			out = append(out, f.byHeight[h][k])
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

// Restore puts back a finalized block a node recorded before it stopped. It is only ever moved
// forward, so an older record cannot undo what the node has since finalized.
func (f *FinalityState) Restore(height uint64, id [32]byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.hasFinal && height <= f.finalized {
		return
	}
	f.finalized, f.finalID, f.hasFinal = height, id, true
	f.pruneBelow(height)
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
