package main

import (
	"errors"
	"fmt"
	"log"
	"runtime/debug"
	"sync"
	"sync/atomic"

	"powmachina/internal/chain"
	"powmachina/internal/mh"
)

// The epoch cache.
//
// Every epoch (5 000 blocks ≈ 6.9 days) has **its own DAG for lane G and its own RandomX key
// for lane C**. A node holding only one stops verifying blocks the moment the chain crosses an
// epoch boundary — and since it rejects the headers, it looks like a stalled network rather
// than a misconfiguration. So epochs are built on demand and at least two are kept.
//
// Why at least two: a reorg across an epoch boundary needs the old one too. `keepEpochs` is
// therefore a minimum, not an optimization.
const keepEpochs = 2

// warmAhead is how many blocks before an epoch boundary the next one starts being built.
//
// Building the full DAG costs seconds and a gigabyte of memory. If it started only with the
// block that already belongs to the new epoch, the node would freeze exactly when new blocks
// are arriving — and at an epoch boundary everybody is in the same position. With a 120 s
// block, 20 blocks is ~40 minutes of margin and it is paid once per epoch (6.9 days).
const warmAhead = 20

// epochSet is everything needed to verify a block of one epoch.
//
// `mh.Hasher` is not safe for concurrent use, so hashing sits behind a mutex. In practice
// there is no contention — PoW is verified once per header.
type epochSet struct {
	mu       sync.Mutex
	epoch    uint64
	verifier *mh.Verifier
	rx       *mh.RandomX
	hasher   *mh.Hasher
}

// VerifyPoW implements chain.PoWChecker.
func (s *epochSet) VerifyPoW(h *chain.Header) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return h.CheckPoW(s.hasher, s.verifier)
}

// Hash computes the header's PoW hash — stratum verifies shares with it (it needs the value, not just yes/no).
func (s *epochSet) Hash(h *chain.Header) ([32]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pow := h.PoWInput()
	return s.hasher.Hash(s.verifier, &pow, h.Nonce, h.Lane)
}

func (s *epochSet) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hasher.Close()
	if s.rx != nil {
		s.rx.Close()
	}
	s.verifier.Close()
}

// dagMode says whether the node holds the full DAG (~1 GiB and growing) or only the cache (light).
type dagMode int

const (
	// dagAuto: the full DAG during the initial sync (hundreds of thousands of headers), light at the tip.
	dagAuto dagMode = iota
	// dagFull: always full — a miner and a pool that hashes itself.
	dagFull
	// dagLight: always light — a machine with little RAM. The initial sync is slower (~2 h per year of history).
	dagLight
)

func parseDagMode(s string) (dagMode, error) {
	switch s {
	case "", "auto":
		return dagAuto, nil
	case "full":
		return dagFull, nil
	case "light":
		return dagLight, nil
	}
	return 0, fmt.Errorf("unknown mode %q (auto, full, light)", s)
}

func (m dagMode) String() string {
	return [...]string{"auto", "full", "light"}[m]
}

type epochCache struct {
	mu      sync.Mutex
	profile mh.DagProfile
	sets    map[uint64]*epochSet
	// order holds the use order, the most recent last
	order []uint64

	mode dagMode
	// syncing (only for dagAuto) says whether the node is catching up on history. It is called
	// under c.mu — it must not take a lock that forHeight is called under.
	syncing func() bool
	// full is the mode the built sets are in right now. Atomic, so a handshake does not wait
	// for an epoch build (seconds under c.mu).
	full atomic.Bool
	// closed is true after close(); further calls return an error instead of writing into a
	// freed map. The node closes even while a connection goroutine is still running.
	closed bool
}

// errEpochsClosed means the node is closing and the epochs have been freed.
var errEpochsClosed = errors.New("the epochs are closed (the node is shutting down)")

func newEpochCache(profile mh.DagProfile) *epochCache {
	c := &epochCache{profile: profile, sets: make(map[uint64]*epochSet)}
	c.full.Store(true)
	return c
}

// isFull says whether the node currently holds the full DAG.
func (c *epochCache) isFull() bool { return c.full.Load() }

// setMode sets the DAG mode. Called before the network starts.
func (c *epochCache) setMode(mode dagMode, syncing func() bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.mode, c.syncing = mode, syncing
}

// wantFullLocked decides whether the DAG should be full. In auto mode it is decided by the age
// of our own tip — which a foreign peer cannot move backwards. The worst that withholding
// blocks (an eclipse) causes is a one-off build of the full DAG; whoever does not have the RAM
// for it runs the node with -dag=light.
func (c *epochCache) wantFullLocked() bool {
	switch c.mode {
	case dagFull:
		return true
	case dagLight:
		return false
	}
	return c.syncing == nil || c.syncing()
}

// switchLocked moves every held set into the requested mode. To light it is instant (the cache
// stays), to full the dataset is generated out of the cache (seconds).
func (c *epochCache) switchLocked(full bool) {
	if c.full.Load() == full {
		return
	}
	c.full.Store(full)
	if full {
		log.Printf("DAG: switching to full (catching up on history)")
	} else {
		log.Printf("DAG: switching to light — the node is at the tip, the full DAG is not needed")
	}
	for _, s := range c.sets {
		s.mu.Lock()
		old := s.verifier
		if full {
			s.verifier = old.Filled()
		} else {
			s.verifier = old.Lightened()
		}
		s.mu.Unlock()
		old.Close()
	}
	// Give the freed dataset (a gigabyte) back to the system right away, not whenever the GC remembers.
	if !full {
		debug.FreeOSMemory()
	}
}

// forHeight returns the set for the epoch of a given height; when it does not have it, it
// builds it.
//
// Building costs seconds (the DAG + the RandomX Argon2d cache) and gigabytes. It is called
// under the lock, so a second goroutine waits instead of building the same thing twice.
func (c *epochCache) forHeight(height uint64) (*epochSet, error) {
	epoch := mh.EpochForHeight(height)
	c.mu.Lock()
	defer c.mu.Unlock()
	// After close an error is returned rather than a panic. The caller is a connection
	// goroutine and it is allowed to end on an error — `Close` does wait for them, but racing
	// with them is worse than one `if`.
	if c.closed {
		return nil, errEpochsClosed
	}
	c.switchLocked(c.wantFullLocked())

	if s, ok := c.sets[epoch]; ok {
		c.touchLocked(epoch)
		return s, nil
	}

	var v *mh.Verifier
	if c.full.Load() {
		log.Printf("building epoch %d (full DAG + RandomX key) — this will take a moment", epoch)
		v = mh.NewVerifier(epoch, c.profile)
	} else {
		log.Printf("building epoch %d (light: DAG cache + RandomX key)", epoch)
		v = mh.NewLightVerifier(epoch, c.profile)
	}
	rx, err := mh.NewRandomX(epoch, false)
	if err != nil {
		v.Close()
		return nil, fmt.Errorf("epoch %d: %w", epoch, err)
	}
	h, err := mh.NewHasherWithRandomX(rx)
	if err != nil {
		rx.Close()
		v.Close()
		return nil, fmt.Errorf("epoch %d: %w", epoch, err)
	}
	s := &epochSet{epoch: epoch, verifier: v, rx: rx, hasher: h}
	c.sets[epoch] = s
	c.touchLocked(epoch)
	c.evictLocked()
	return s, nil
}

func (c *epochCache) touchLocked(epoch uint64) {
	for i, e := range c.order {
		if e == epoch {
			c.order = append(c.order[:i], c.order[i+1:]...)
			break
		}
	}
	c.order = append(c.order, epoch)
}

func (c *epochCache) evictLocked() {
	for len(c.order) > keepEpochs {
		old := c.order[0]
		c.order = c.order[1:]
		if s, ok := c.sets[old]; ok {
			delete(c.sets, old)
			// Freeing outside the cache lock would be nicer, but the set is no longer
			// reachable and `close` takes a mutex of its own, so no cycle arises here.
			s.close()
			log.Printf("epoch %d freed (keeping the %d most recent)", old, keepEpochs)
		}
	}
}

// warm builds an epoch ahead of time. Called when the tip approaches a boundary, so the node
// does not freeze for seconds at the turn exactly when new blocks are arriving.
func (c *epochCache) warm(height uint64) {
	if _, err := c.forHeight(height); err != nil {
		log.Printf("⚠ the epoch for height %d could not be prepared: %v", height, err)
	}
}

func (c *epochCache) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, s := range c.sets {
		s.close()
	}
	c.sets = nil
	c.order = nil
	c.closed = true
}

// warmNextEpoch builds the next epoch ahead of time once the tip has come within warmAhead
// blocks of it. It is built in the background: on a block's hot path it would be a delay of
// seconds exactly when further blocks are arriving.
//
// Without this the node freezes for seconds at an epoch boundary, and since everybody is in the
// same position at the boundary, it looks like a stalled network.
func (n *Node) warmNextEpoch(height uint64) {
	next := mh.EpochForHeight(height + warmAhead)
	if next == mh.EpochForHeight(height) {
		return
	}
	n.mu.Lock()
	if n.warmedEpoch == next {
		n.mu.Unlock()
		return // already being built or already built
	}
	n.warmedEpoch = next
	n.mu.Unlock()

	target := next * mh.EpochBlocks
	log.Printf("epoch %d is approaching (tip %d) — building it ahead", next, height)
	n.goTracked(func() { n.epochs.warm(target) })
}
