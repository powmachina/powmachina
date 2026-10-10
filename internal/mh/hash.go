// Package mh is MachinaHash MH-1 — PowMachina's PoW algorithm.
//
// This is the **only** implementation of the algorithm in the project. The node calls it directly,
// the GPU kernels are validated against it with test vectors (vectors/mh-test-vectors.txt). A second
// independent PoW implementation must not be written — a difference of one constant = a network fork.
//
// Two lanes, two algorithms:
//
//	| lane | algorithm                                       | domain |
//	|------|-------------------------------------------------|--------|
//	|  C   | MachinaHash-C — RandomX v2 with PowMachina salt  |  CPU   |
//	|  G   | MachinaHash-G — B-core over the epoch DAG        |  GPU   |
//
// There is no hybrid CPU+GPU lane (the algorithm spec §7.7). Its single purpose would be for a
// balanced rig to beat the same hardware split into C+G — and with RandomX that is unachievable in
// principle: RandomX is so heavy as the CPU half that the GPU half is free against it. A tandem
// comes out 11× worse than C and G separately, for any split of the work.
//
// RandomX itself is vendored, not rewritten — see internal/randomx.
package mh

import (
	"encoding/binary"
	"errors"
	"fmt"
	"runtime"
	"sync"

	"github.com/zeebo/blake3"

	"powmachina/internal/randomx"
)

// Lane is a mining lane. It is part of the PoW input, so a nonce found in one lane is useless in
// another.
type Lane uint8

const (
	LaneC Lane = 0 // MachinaHash-C: RandomX v2 with PowMachina salt
	LaneG Lane = 1 // MachinaHash-G: B-core over the epoch DAG
)

// Lanes is the order the lanes are listed in. Consensus knows exactly this many.
var Lanes = [...]Lane{LaneC, LaneG}

func (l Lane) String() string {
	switch l {
	case LaneC:
		return "C"
	case LaneG:
		return "G"
	default:
		return fmt.Sprintf("lane(%d)", uint8(l))
	}
}

const (
	// PowPreimageBytes is the PoW preimage: the header ‖ nonce (LE) ‖ lane. **89 bytes.**
	//
	// Lane C hands it to RandomX directly — the way rx/pcoin or rx/scash hand it a Bitcoin header.
	// Thanks to that a miner only has to know one thing: **the nonce is at offset 80**. No rehashing
	// before RandomX, no CryptoNote blob.
	PowPreimageBytes = HeaderBytes + 8 + 1
	NonceOffset      = HeaderBytes
	LaneOffset       = HeaderBytes + 8

	// GBRounds and GBAccesses are the B-core rounds and accesses for lane G.
	GBRounds   uint32 = 16
	GBAccesses        = 4 * BAccessesPerRound
)

// PowPreimage assembles the 89-byte preimage.
func PowPreimage(header *[HeaderBytes]byte, nonce uint64, lane Lane) [PowPreimageBytes]byte {
	var p [PowPreimageBytes]byte
	copy(p[:HeaderBytes], header[:])
	binary.LittleEndian.PutUint64(p[NonceOffset:], nonce)
	p[LaneOffset] = uint8(lane)
	return p
}

// RxEpochKey is the RandomX key for a given epoch.
//
// Derived **from the height only**, not from a block hash. Monero takes the key from an older block
// for historical reasons, but the dataset is public and deterministic anyway, so nothing is gained by
// it — and the question of what to do with a reorg across an epoch boundary disappears. A miner can
// build the key arbitrarily far ahead.
func RxEpochKey(epoch uint64) [32]byte {
	var epochLE [8]byte
	binary.LittleEndian.PutUint64(epochLE[:], epoch)
	h := blake3.New()
	_, _ = h.Write(domainRxKey)
	_, _ = h.Write(epochLE[:])
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// LaneWork is lane G's work per hash — for balancing and for checks in tests.
type LaneWork struct {
	BRounds  uint32
	DagBytes int
}

// LaneGWork returns how much lane G reads from the DAG per hash.
func LaneGWork() LaneWork {
	return LaneWork{BRounds: GBRounds, DagBytes: int(GBRounds) * GBAccesses * DagItemBytes}
}

// ---------------------------------------------------------------- errors

// ErrNoRandomX means lane C is being asked of a hasher without a RandomX context. It is the caller's
// bug, not a data error — a node verifying lane C has to have the epoch's context.
var ErrNoRandomX = errors.New("mh: lane C needs a RandomX context (NewHasherWithRandomX)")

// ErrRandomXInit means RandomX could not be initialized. In fast mode it is usually a lack of memory
// (~2.1 GiB), in light mode ~256 MiB.
var ErrRandomXInit = errors.New("mh: RandomX could not be initialized")

// ---------------------------------------------------------------- verifier

// DagProfile says how large the DAG and its cache should be. Zero values = the epoch's mainnet
// profile.
type DagProfile struct {
	CacheBytes   uint64
	DatasetBytes uint64
}

// TestProfile is the test profile (a 1 MiB cache, an 8 MiB dataset).
var TestProfile = DagProfile{CacheBytes: 1 << 20, DatasetBytes: 8 << 20}

// MainnetProfile leaves the sizes up to the epoch.
var MainnetProfile = DagProfile{}

// Params are the cache and dataset sizes for an epoch — for a GPU miner, which builds the dataset itself.
func (p DagProfile) Params(epoch uint64) DagParams { return p.params(epoch) }

func (p DagProfile) params(epoch uint64) DagParams {
	q := MainnetDag(epoch)
	if p.CacheBytes != 0 {
		q.CacheBytes = int(p.CacheBytes)
	}
	if p.DatasetBytes != 0 {
		q.DatasetBytes = int(p.DatasetBytes)
	}
	return q
}

// Verifier holds the DAG and the RANDMATH sequence for one epoch. It is safe for concurrent use by
// several Hashers, but it does not compute hashes itself.
//
// Two modes, **one algorithm**: a full verifier reads items from the dataset in memory (~1 GiB,
// ~1 ms per lane G hash), a light one computes them from the cache (~26 MiB, ~60–100 ms per hash). In
// both cases an item is computed by the same function (`Cache.datasetItem`) — the full dataset is just
// its precomputed result. The agreement is verified by TestLightAndFullDagAgree and
// TestLightVerifierMatchesFull.
type Verifier struct {
	epoch uint64
	cache *Cache
	dag   *FullDag // nil in light mode
	math  *RandMath

	// The light path holds a workspace, which is why it is behind a lock — concurrent verification
	// in light mode goes one at a time (on a light node that is also the cap on the CPU foreign
	// headers can take from it: one core).
	lightMu sync.Mutex
	light   *LightDag
}

// NewVerifier builds the full DAG for a given epoch — for a miner and for the initial sync, where
// hundreds of thousands of headers are verified (the algorithm spec §2.2).
func NewVerifier(epoch uint64, p DagProfile) *Verifier {
	return newFullVerifier(epoch, BuildCache(p.params(epoch)), NewRandMath(epoch))
}

// NewLightVerifier builds the cache only. For a node that does not mine and is at the tip: it
// verifies a block every two minutes in tens of milliseconds without a gigabyte of RAM.
func NewLightVerifier(epoch uint64, p DagProfile) *Verifier {
	return newLightVerifier(epoch, BuildCache(p.params(epoch)), NewRandMath(epoch))
}

// NewLightVerifierFrom builds a light verifier over an already built cache (a GPU miner needs it
// anyway, so it is not built twice).
func NewLightVerifierFrom(epoch uint64, c *Cache) *Verifier {
	return newLightVerifier(epoch, c, NewRandMath(epoch))
}

func newFullVerifier(epoch uint64, c *Cache, m *RandMath) *Verifier {
	return &Verifier{epoch: epoch, cache: c, dag: BuildFullDagFrom(c), math: m}
}

func newLightVerifier(epoch uint64, c *Cache, m *RandMath) *Verifier {
	return &Verifier{epoch: epoch, cache: c, math: m, light: NewLightDagFrom(c)}
}

// Light says whether the verifier runs without the full dataset.
func (v *Verifier) Light() bool { return v.dag == nil }

// Lightened returns a light verifier of the same epoch over the same cache — instantly, without
// recomputing. The original verifier stays valid until the caller closes it.
func (v *Verifier) Lightened() *Verifier {
	return newLightVerifier(v.epoch, v.cache, v.math)
}

// Filled returns a full verifier of the same epoch — it generates the dataset from the already built
// cache (seconds, all cores).
func (v *Verifier) Filled() *Verifier {
	return newFullVerifier(v.epoch, v.cache, v.math)
}

func (v *Verifier) Epoch() uint64 { return v.epoch }

// Dag exposes the dataset — for tools that compare it with the GPU (F3). In light mode it is nil.
func (v *Verifier) Dag() *FullDag { return v.dag }

// Math exposes the RANDMATH sequence — the GPU kernel receives it as a constant.
func (v *Verifier) Math() *RandMath { return v.math }

// Close frees the dataset. After it the Verifier must not be used.
func (v *Verifier) Close() {
	v.lightMu.Lock()
	defer v.lightMu.Unlock()
	v.dag = nil
	v.light = nil
	v.cache = nil
	v.math = nil
}

// ---------------------------------------------------------------- RandomX

// RandomX holds the cache (and in fast mode the dataset) for one epoch's key. It is built once per
// epoch, not per hash, and it is safe for concurrent use by several Hashers.
type RandomX struct {
	epoch uint64
	fast  bool
	ctx   *randomx.KeyContext
}

// NewRandomX builds the context for an epoch in the verifier role (W^X always). fast=false is enough
// for a node (the cache only, ~256 MiB); fast=true is for a miner (the full dataset, ~2.1 GiB).
func NewRandomX(epoch uint64, fast bool) (*RandomX, error) {
	return newRandomX(epoch, fast, randomx.Verifier, true)
}

// NewRandomXForMiner is the version for a miner: it hashes its own data, so it may turn W^X off (on
// 96 threads that is a 2.17× difference). See randomx.Role.
func NewRandomXForMiner(epoch uint64, fast, secure, largePages bool) (*RandomX, error) {
	role := randomx.MinerFast
	if secure {
		role = randomx.MinerSecure
	}
	return newRandomX(epoch, fast, role, largePages)
}

func newRandomX(epoch uint64, fast bool, role randomx.Role, largePages bool) (*RandomX, error) {
	mode := randomx.Light
	if fast {
		mode = randomx.Fast
	}
	key := RxEpochKey(epoch)
	ctx, err := randomx.NewKeyContextAs(key[:], mode, role, largePages)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrRandomXInit, err)
	}
	return &RandomX{epoch: epoch, fast: fast, ctx: ctx}, nil
}

func (r *RandomX) Epoch() uint64 { return r.epoch }
func (r *RandomX) Fast() bool    { return r.fast }

// UsesLargePages says whether the allocation in large pages succeeded.
func (r *RandomX) UsesLargePages() bool { return r.ctx.UsesLargePages() }

// FlagsString is the list of flags the context runs with — for diagnostics.
func (r *RandomX) FlagsString() string { return r.ctx.FlagsString() }

func (r *RandomX) Close() {
	if r.ctx != nil {
		r.ctx.Close()
		r.ctx = nil
	}
}

// JITIsSecure says whether the RandomX JIT runs with W^X enforced (or does not run at all). A node
// that processes blocks from strangers should verify this at startup.
func JITIsSecure() bool { return randomx.JITIsSecure() }

// HasHardwareAES says whether the CPU has hardware AES. Without it lane C is several times slower.
func HasHardwareAES() bool { return randomx.HasHardwareAES() }

// RandomXFlags is the list of RandomX's flags — for diagnostics at node startup.
func RandomXFlags() string { return randomx.FlagsString() }

// ---------------------------------------------------------------- hasher

// Hasher is the state for one thread. It is not safe for concurrent use.
type Hasher struct {
	b  *bcoreState
	vm *randomx.VM
	rx *RandomX // keeps the context alive as long as the hasher lives
}

// NewHasher builds a hasher that can compute lane G only.
func NewHasher() *Hasher { return &Hasher{b: newBcoreState()} }

// NewHasherWithRandomX builds a hasher for both lanes. rx has to outlive the hasher — the reference
// in the struct takes care of that.
func NewHasherWithRandomX(rx *RandomX) (*Hasher, error) {
	vm, err := rx.ctx.NewVM()
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrRandomXInit, err)
	}
	h := &Hasher{b: newBcoreState(), vm: vm, rx: rx}
	runtime.SetFinalizer(h, (*Hasher).Close)
	return h, nil
}

func (h *Hasher) Close() {
	if h.vm != nil {
		h.vm.Close()
		h.vm = nil
		runtime.SetFinalizer(h, nil)
	}
}

// Hash computes the PoW for a given header, nonce and lane. The verifier and any RandomX context have
// to be for the same epoch — the caller takes care of that.
func (h *Hasher) Hash(v *Verifier, header *[HeaderBytes]byte, nonce uint64, lane Lane) ([32]byte, error) {
	p := PowPreimage(header, nonce, lane)
	switch lane {
	case LaneG:
		if v.dag != nil {
			return h.laneG(&p, v.dag, v.math), nil
		}
		v.lightMu.Lock()
		defer v.lightMu.Unlock()
		if v.light == nil {
			return [32]byte{}, errors.New("mh: the verifier is closed")
		}
		return h.laneG(&p, v.light, v.math), nil
	case LaneC:
		if h.vm == nil {
			return [32]byte{}, ErrNoRandomX
		}
		out := h.vm.Hash(p[:])
		runtime.KeepAlive(h.rx)
		return out, nil
	default:
		return [32]byte{}, fmt.Errorf("mh: unknown lane %d", uint8(lane))
	}
}

// HashWithDag is Hash over any DagAccess implementation — for the light path and for tools. The node
// uses Hash.
func (h *Hasher) HashWithDag(dag DagAccess, math *RandMath, header *[HeaderBytes]byte, nonce uint64, lane Lane) ([32]byte, error) {
	p := PowPreimage(header, nonce, lane)
	switch lane {
	case LaneG:
		return h.laneG(&p, dag, math), nil
	case LaneC:
		if h.vm == nil {
			return [32]byte{}, ErrNoRandomX
		}
		out := h.vm.Hash(p[:])
		runtime.KeepAlive(h.rx)
		return out, nil
	default:
		return [32]byte{}, fmt.Errorf("mh: unknown lane %d", uint8(lane))
	}
}

// HashLaneC is lane C without a Verifier — RandomX does not go into the DAG, so it does not need one.
// For the miner loop and for calibration.
func (h *Hasher) HashLaneC(header *[HeaderBytes]byte, nonce uint64) ([32]byte, error) {
	if h.vm == nil {
		return [32]byte{}, ErrNoRandomX
	}
	p := PowPreimage(header, nonce, LaneC)
	out := h.vm.Hash(p[:])
	runtime.KeepAlive(h.rx)
	return out, nil
}

// LaneCPipe is pipelined lane C hashing for a miner: Start(nonce) and then Next(the next one) returns
// the hash of the previous nonce. The result is bit for bit the same as HashLaneC.
type LaneCPipe struct {
	h    *Hasher
	hdr  *[HeaderBytes]byte
	next [PowPreimageBytes]byte
}

// PipeLaneC prepares pipelined hashing over a header.
func (h *Hasher) PipeLaneC(header *[HeaderBytes]byte) (*LaneCPipe, error) {
	if h.vm == nil {
		return nil, ErrNoRandomX
	}
	return &LaneCPipe{h: h, hdr: header}, nil
}

// Start begins the hash for `nonce`.
func (p *LaneCPipe) Start(nonce uint64) {
	p.next = PowPreimage(p.hdr, nonce, LaneC)
	p.h.vm.HashFirst(p.next[:])
}

// Next returns the hash of the nonce from the previous call (Start or Next) and begins `nonce`.
func (p *LaneCPipe) Next(nonce uint64, out *[32]byte) {
	p.next = PowPreimage(p.hdr, nonce, LaneC)
	p.h.vm.HashNext(p.next[:], out)
	runtime.KeepAlive(p.h.rx)
}

// laneG: the seed from the preimage, B-core over the DAG, a domain-separated output.
func (h *Hasher) laneG(p *[PowPreimageBytes]byte, dag DagAccess, math *RandMath) [32]byte {
	seed := h.b.x.sum32(domainSeed, p[:])
	state := h.b.run(&seed, GBRounds, GBAccesses, dag, math)

	return h.b.x.sum32(domainFinal, state[:], seed[:], []byte{uint8(LaneG)})
}

// ---------------------------------------------------------------- the rest

// MeetsTarget compares a PoW result with a target. Both are 256-bit big-endian.
func MeetsTarget(hash, target *[32]byte) bool {
	for i := range hash {
		switch {
		case hash[i] < target[i]:
			return true
		case hash[i] > target[i]:
			return false
		}
	}
	return true
}

// BlockID is BLAKE3(domain ‖ the serialized header).
func BlockID(data []byte) [32]byte {
	var out [32]byte
	if len(data) == 0 {
		return out
	}
	h := blake3.New()
	_, _ = h.Write(domainBlockID)
	_, _ = h.Write(data)
	copy(out[:], h.Sum(nil))
	return out
}

// TaggedHash is BLAKE3(tag ‖ data). The node uses it for transaction IDs and merkle nodes.
// The domain (the tag) separates the uses, so a hash from one context cannot be passed off as another.
func TaggedHash(tag string, data []byte) [32]byte {
	h := blake3.New()
	_, _ = h.WriteString(tag)
	_, _ = h.Write(data)
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}
