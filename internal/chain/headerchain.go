package chain

import (
	"errors"
	"fmt"
	"math/big"
	"sort"
	"sync"
	"time"

	"powmachina/internal/mh"
)

// HeaderChain is the state of the header chain: what the node knows, which branch is its active
// one and how much work stands behind it.
//
// Header validation sits deliberately in one place and in this order: the cheap checks first
// (linkage, height, time, difficulty, lane rotation) and PoW, which costs ~1.7 ms, last. The
// opposite order would turn every invalid header into free work for the node.

var (
	ErrUnknownParent  = errors.New("chain: the header's predecessor is not known")
	ErrBadHeight      = errors.New("chain: the height does not follow its predecessor")
	ErrTimeTooOld     = errors.New("chain: the time is not above the median time past")
	ErrTimeTooNew     = errors.New("chain: the time is too far in the future")
	ErrBadBits        = errors.New("chain: the difficulty does not match the retarget")
	ErrLaneNotAllowed = errors.New("chain: lane rotation rule — three identical lanes in a row")
	ErrBadPoW         = errors.New("chain: the header does not meet its target")
	// ErrForkTooDeep: the branch forks deeper than ReorgLimit allows (or below finality) —
	// it would never be switched to, so there is no point even storing it.
	ErrForkTooDeep = errors.New("chain: the branch forks deeper than a reorg may go")
	// ErrCheckpoint: the header at a checkpoint height has a different ID than the one baked into the binary.
	ErrCheckpoint = errors.New("chain: the header does not match the checkpoint")
	ErrDuplicate  = errors.New("chain: we already have the header")
)

type node struct {
	header *Header
	id     [32]byte
	work   *big.Int // cumulative normalized chainwork
	height uint64
	// lastOf is the most recent block of each lane at or below this one, the genesis not counted;
	// first is this branch's block at height 1. The lane retarget reads them (asert.go) — without
	// them it would walk back through the whole chain for a lane that has not mined for a while.
	lastOf [len(mh.Lanes)]*node
	first  *node
	// tainted: this block or one below it is marked invalid. A header is only filed when its
	// branch is clean, so it starts false; an invalidation sets it on everything above the mark
	// (retaintLocked). Asking it is one load — walking down to the genesis for every header was
	// a step per block of history, and a sync of the whole chain grew with its square.
	tainted bool
}

// link fills in what a node inherits from its parent. Every node but the genesis goes through it.
func (n *node) link(parent *node) {
	n.lastOf, n.first = parent.lastOf, parent.first
	if n.first == nil {
		n.first = n
	}
	if int(n.header.Lane) < len(n.lastOf) {
		n.lastOf[n.header.Lane] = n
	}
}

// HeaderChain is safe for concurrent use.
type HeaderChain struct {
	mu     sync.RWMutex
	params NetParams
	byID   map[[32]byte]*node
	// invalid are the blocks whose body failed validation. A child of an invalid block is
	// invalid too — see invalid.go.
	invalid map[[32]byte]bool
	// parked are the blocks whose body does not come. Neither they nor what grows out of them
	// can be the tip — see parked.go.
	parked   map[[32]byte]bool
	tip      *node
	genesis  [32]byte
	finality *FinalityState
	now      func() time.Time
	// votes count the share units seen built on a block (siblings.go); nil counts none.
	votes func(id [32]byte) uint64
}

// GenesisID is the root this chain stands on. It is set once in the constructor and never
// changes, so no lock is needed.
//
// It has a single use on the outside and it is an important one: a node on a **different
// network** cannot be recognized otherwise. The same magic with a different genesis means the
// peers shake hands and then reject each other's blocks — which looks like a network fault in
// the logs.
func (hc *HeaderChain) GenesisID() [32]byte { return hc.genesis }

// NewHeaderChain creates a chain with the given genesis block. The genesis is not verified — it
// is the root of trust written into the binary.
func NewHeaderChain(params NetParams, genesis *Header, finality *FinalityState) *HeaderChain {
	g := &node{header: genesis, id: genesis.ID(), work: big.NewInt(0), height: uint64(genesis.Height)}
	hc := &HeaderChain{
		params:   params,
		byID:     map[[32]byte]*node{g.id: g},
		tip:      g,
		genesis:  g.id,
		finality: finality,
		now:      time.Now,
	}
	return hc
}

func (hc *HeaderChain) Tip() (*Header, uint64, [32]byte) {
	hc.mu.RLock()
	defer hc.mu.RUnlock()
	return hc.tip.header, hc.tip.height, hc.tip.id
}

// Work returns the cumulative normalized chainwork of the active branch.
// TrustedWork says whether the active branch has at least as much work as the network expects
// (NetParams.MinChainWork). On a network without that value it always returns true.
//
// It is not a consensus rule — headers keep being accepted, otherwise the node would have no way
// to sync. It is the answer to the question "may I trust this chain", which the wallet, an
// exchange and the operator all ask.
func (hc *HeaderChain) TrustedWork() bool {
	min := hc.params.MinWork()
	if min == nil {
		return true
	}
	hc.mu.RLock()
	defer hc.mu.RUnlock()
	return hc.tip.work.Cmp(min) >= 0
}

// anchoredLocked says whether the active branch is one the node may defend: it has passed the last
// checkpoint and has at least the work the network had when the binary was made.
//
// Until then the node is on its first download and knows nothing about the chain it holds except that
// somebody sent it. The rules that protect a chain against being replaced — the fork depth limit, MESS
// — must not protect that one: the first peer to answer could hand over a few hundred cheap headers
// and the node would then refuse the real chain for forking too deep, for good. An unanchored node
// follows the most work, from any depth.
//
// On a network with no checkpoint and no MinChainWork every chain is anchored: there is nothing to
// measure it against.
func (hc *HeaderChain) anchoredLocked() bool {
	if min := hc.params.MinWork(); min != nil && hc.tip.work.Cmp(min) < 0 {
		return false
	}
	for _, cp := range hc.params.Checkpoints {
		if hc.tip.height < cp.Height {
			return false
		}
	}
	return true
}

// Anchored is anchoredLocked for callers outside.
func (hc *HeaderChain) Anchored() bool {
	hc.mu.RLock()
	defer hc.mu.RUnlock()
	return hc.anchoredLocked()
}

func (hc *HeaderChain) Work() *big.Int {
	hc.mu.RLock()
	defer hc.mu.RUnlock()
	return new(big.Int).Set(hc.tip.work)
}

func (hc *HeaderChain) Has(id [32]byte) bool {
	hc.mu.RLock()
	defer hc.mu.RUnlock()
	_, ok := hc.byID[id]
	return ok
}

// HeaderByID returns a header we have verified. It exists for the RPC: a block list has to be
// able to show a row even when the body has not arrived yet — height, lane, time and bits are
// all in the header, so there is no reason for the height to disappear from the list.
func (hc *HeaderChain) HeaderByID(id [32]byte) (*Header, bool) {
	hc.mu.RLock()
	defer hc.mu.RUnlock()
	n, ok := hc.byID[id]
	if !ok {
		return nil, false
	}
	return n.header, true
}

// BlockIDAt returns the ID of the block at a given height on the active branch — that is `ChainLookup` for finality.
func (hc *HeaderChain) BlockIDAt(height uint64) ([32]byte, bool) {
	hc.mu.RLock()
	defer hc.mu.RUnlock()
	for n := hc.tip; n != nil; n = hc.byID[n.header.PrevBlock] {
		if n.height == height {
			return n.id, true
		}
		if n.height < height || n.id == hc.genesis {
			break
		}
	}
	return [32]byte{}, false
}

// PoWChecker verifies a header's PoW. It is an interface rather than a (hasher, verifier) pair,
// because **every epoch has its own DAG and its own RandomX key**. Whoever adds a header has to
// be able to supply the right ones for its height — and only the caller holding the epoch cache knows them.
type PoWChecker interface {
	VerifyPoW(h *Header) (bool, error)
}

// PoWPair is the simplest PoWChecker: a fixed pair for one epoch. For tests and for chains that
// do not cross an epoch boundary.
type PoWPair struct {
	Hasher   *mh.Hasher
	Verifier *mh.Verifier
}

func (p PoWPair) VerifyPoW(h *Header) (bool, error) { return h.CheckPoW(p.Hasher, p.Verifier) }

// CheckHeader runs every check that needs no PoW computation: the parent is known, the height follows
// it, the branch is one the node would take, the time, the difficulty and the lane are right. It files
// nothing.
//
// It is what a caller asks before it fetches the PoW verifier for the header's height. The verifier
// belongs to an epoch and an epoch costs gigabytes to build, so it is built only for a header that got
// this far — never for a height a peer merely wrote into a header.
func (hc *HeaderChain) CheckHeader(h *Header) error {
	hc.mu.Lock()
	defer hc.mu.Unlock()
	_, err := hc.checkLocked(h, h.ID())
	return err
}

// checkLocked is the cheap part of AddHeader. It returns the parent.
func (hc *HeaderChain) checkLocked(h *Header, id [32]byte) (*node, error) {
	if _, dup := hc.byID[id]; dup {
		return nil, ErrDuplicate
	}
	parent, ok := hc.byID[h.PrevBlock]
	if !ok {
		return nil, ErrUnknownParent
	}
	if uint64(h.Height) != parent.height+1 {
		return nil, ErrBadHeight
	}

	// A header from an invalid branch goes away right away: we paid for it once by downloading
	// the body and validating it, we will not pay a second time.
	if hc.invalid[id] || parent.tainted {
		return nil, ErrInvalidAncestor
	}
	if cp, ok := hc.params.checkpointAt(uint64(h.Height)); ok && cp != id {
		return nil, fmt.Errorf("%w at height %d", ErrCheckpoint, h.Height)
	}
	// A side branch may fork at most ReorgLimit below the tip (and not below finality). The node
	// would never accept a deeper one anyway — and storing it would let an attacker fill memory
	// with cheap headers from early heights, where the difficulty was low.
	//
	// Only for a chain the node may defend (anchoredLocked): below the last checkpoint the limit
	// would be measured from whatever the first peer sent.
	if parent != hc.tip && hc.anchoredLocked() {
		fork := hc.forkPointBoundedLocked(parent, ReorgLimit)
		if fork == nil {
			return nil, ErrForkTooDeep
		}
		if hc.finality != nil && hc.finality.CheckReorg(fork.height) != nil {
			return nil, ErrForkTooDeep
		}
	}
	if h.Time < hc.minTimeLocked(parent) {
		return nil, ErrTimeTooOld
	}
	if int64(h.Time) > hc.now().Add(hc.params.FutureDrift(uint64(h.Height))).Unix() {
		return nil, ErrTimeTooNew
	}
	wantBits, err := hc.nextBitsLocked(parent, h.Lane)
	if err != nil {
		return nil, err
	}
	if h.Bits != wantBits {
		return nil, fmt.Errorf("%w: got %08x, expected %08x", ErrBadBits, h.Bits, wantBits)
	}
	// Against the PARENT's time, not against the median time past: the MTP trails the tip by
	// ~6 blocks, so the relief would apply even on a perfectly healthy chain and the rotation
	// rule would do nothing.
	if !LaneAllowedAt(hc.recentLanesLocked(parent, MaxSameLaneRun), h.Lane,
		time.Unix(int64(h.Time), 0), time.Unix(int64(parent.header.Time), 0)) {
		return nil, ErrLaneNotAllowed
	}
	return parent, nil
}

// AddHeader verifies and files a header. It returns true if that moved the tip.
func (hc *HeaderChain) AddHeader(h *Header, pow PoWChecker) (bool, error) {
	hc.mu.Lock()
	defer hc.mu.Unlock()

	// --- the cheap checks ---
	id := h.ID()
	parent, err := hc.checkLocked(h, id)
	if err != nil {
		return false, err
	}

	// --- the expensive check last ---
	okPoW, err := pow.VerifyPoW(h)
	if err != nil {
		return false, err
	}
	if !okPoW {
		return false, ErrBadPoW
	}

	work, err := hc.normalizedWorkLocked(parent, h)
	if err != nil {
		return false, err
	}
	n := &node{header: h, id: id, work: new(big.Int).Add(parent.work, work), height: uint64(h.Height)}
	n.link(parent)
	hc.byID[id] = n

	// Extending the active branch.
	if h.PrevBlock == hc.tip.id {
		hc.tip = n
		return true, nil
	}
	// A header above a parked block is filed and waits with it.
	if hc.hasParkedAncestorLocked(n) {
		return false, nil
	}
	// A sibling of the tip: the votes and the lower ID decide, not the work (siblings.go).
	if isSiblingLocked(n, hc.tip) {
		if !hc.prefersSiblingLocked(n, hc.tip) {
			return false, nil
		}
		hc.tip = n
		return true, nil
	}
	return hc.maybeReorgLocked(n)
}

// maybeReorgLocked decides about switching to a competing branch — through MESS and with respect to finality.
func (hc *HeaderChain) maybeReorgLocked(candidate *node) (bool, error) {
	if !hc.anchoredLocked() {
		// A chain that has not reached the checkpoints has no first-seen advantage and no depth
		// to hide behind: more work wins.
		if candidate.work.Cmp(hc.tip.work) <= 0 {
			return false, nil
		}
		hc.tip = candidate
		return true, nil
	}
	fork := hc.forkPointBoundedLocked(candidate, ReorgLimit+1)
	if fork == nil {
		return false, nil
	}
	if hc.finality != nil {
		if err := hc.finality.CheckReorg(fork.height); err != nil {
			return false, err
		}
	}
	age := int64(hc.tip.header.Time) - int64(fork.header.Time)
	localWork := new(big.Int).Sub(hc.tip.work, fork.work)
	candWork := new(big.Int).Sub(candidate.work, fork.work)
	if !AcceptsReorg(localWork, candWork, age) {
		return false, nil
	}
	hc.tip = candidate
	return true, nil
}

// forkPointBoundedLocked finds the common ancestor of `from` and the active branch if it lies at
// most `limit` blocks below the tip. Otherwise it returns nil.
//
// It takes on the order of `limit` steps at most — unlike forkPointLocked, which walks the whole
// active branch down to the genesis. With millions of blocks that would be a million steps for
// every header from a side branch, which is a cheap way to keep a node busy.
func (hc *HeaderChain) forkPointBoundedLocked(from *node, limit uint64) *node {
	tip := hc.tip
	// 1. Come down to the tip's height. As many steps as the branch is longer than ours — each of
	//    them is a header with valid PoW at the current difficulty, so an attacker cannot inflate it cheaply.
	a := from
	for a != nil && a.height > tip.height {
		a = hc.byID[a.header.PrevBlock]
	}
	if a == nil {
		return nil
	}
	// The fork point is at most at height `a`; if that is already deeper than the limit, stop right away.
	if tip.height > limit && a.height+limit < tip.height {
		return nil
	}
	// 2. Bring the tip down to the same height — at most `limit` steps, thanks to the check above.
	b := tip
	for b != nil && b.height > a.height {
		b = hc.byID[b.header.PrevBlock]
	}
	// 3. Walk down together until they meet — at most `limit` steps.
	for steps := uint64(0); a != nil && b != nil && a != b; steps++ {
		if steps >= limit || a.id == hc.genesis || b.id == hc.genesis {
			return nil
		}
		a = hc.byID[a.header.PrevBlock]
		b = hc.byID[b.header.PrevBlock]
	}
	if a == nil || b == nil || a != b {
		return nil
	}
	if tip.height > limit && a.height+limit < tip.height {
		return nil
	}
	return a
}

func (hc *HeaderChain) medianTimePastLocked(from *node) uint32 {
	times := make([]uint32, 0, MedianTimeSpan)
	for n := from; n != nil && len(times) < MedianTimeSpan; n = hc.byID[n.header.PrevBlock] {
		times = append(times, n.header.Time)
		if n.id == hc.genesis {
			break
		}
	}
	sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })
	return times[len(times)/2]
}

// recentLanesLocked returns the lanes of the most recent blocks, the newest first — the input for
// the rotation rule.
//
// **The genesis does not count.** Its lane is a constant from the binary, not the result of
// mining; if it counted, the first real block of the same lane would be "second in a row" and the
// other lane would have to come right after it. A number we made up would then decide about the
// network's first blocks. (The genesis likewise does not count towards the hashrate or the lane stats.)
func (hc *HeaderChain) recentLanesLocked(from *node, count int) []mh.Lane {
	lanes := make([]mh.Lane, 0, count)
	for n := from; n != nil && len(lanes) < count; n = hc.byID[n.header.PrevBlock] {
		if n.id == hc.genesis {
			break
		}
		lanes = append(lanes, n.header.Lane)
	}
	return lanes
}

// nextBitsLocked computes the expected difficulty for a given lane on top of a given predecessor.
func (hc *HeaderChain) nextBitsLocked(parent *node, lane mh.Lane) (uint32, error) {
	if hc.params.Active(ForkLaneDifficulty, parent.height+1) {
		return hc.asertBitsLocked(parent, lane)
	}
	window := make([]LaneBlock, 0, LwmaWindow+1)
	for n := parent; n != nil && len(window) < LwmaWindow+1; n = hc.byID[n.header.PrevBlock] {
		if n.header.Lane == lane {
			window = append(window, LaneBlock{Time: n.header.Time, Bits: n.header.Bits})
		}
		if n.id == hc.genesis {
			break
		}
	}
	if len(window) < 2 {
		// The lane has no history yet — the genesis block's initial difficulty is used.
		return hc.byID[hc.genesis].header.Bits, nil
	}
	// window goes from the newest; NextBits expects it from the oldest.
	for i, j := 0, len(window)-1; i < j; i, j = i+1, j-1 {
		window[i], window[j] = window[j], window[i]
	}
	return NextBits(window, hc.params.PowLimitBits)
}

// normalizedWorkLocked computes a block's work normalized across the lanes (SPEC §3, work.go).
func (hc *HeaderChain) normalizedWorkLocked(parent *node, h *Header) (*big.Int, error) {
	avg := make(map[mh.Lane]*big.Int)
	cnt := make(map[mh.Lane]int64)
	// 12 h at a 30 s block (it was 2 days at 120 s). Kept in blocks when the block got shorter: an
	// average of the lanes' difficulty needs hours, not days, and every header walks the window, so
	// four times the window would be four times the work of a sync.
	const window = 1440
	seen := 0
	for n := parent; n != nil && seen < window; n = hc.byID[n.header.PrevBlock] {
		w, err := Work(n.header.Bits)
		if err == nil {
			l := n.header.Lane
			if avg[l] == nil {
				avg[l] = big.NewInt(0)
			}
			avg[l].Add(avg[l], w)
			cnt[l]++
		}
		seen++
		if n.id == hc.genesis {
			break
		}
	}
	// A lane with no history gets its work from the genesis difficulty, so an average can be computed.
	fallback, err := Work(hc.byID[hc.genesis].header.Bits)
	if err != nil {
		return nil, err
	}
	get := func(l mh.Lane) *big.Int {
		if cnt[l] == 0 {
			return fallback
		}
		return new(big.Int).Div(avg[l], big.NewInt(cnt[l]))
	}
	geo := GeoMean2(get(mh.LaneC), get(mh.LaneG))
	return NormalizedWork(h.Bits, get(h.Lane), geo)
}

// NextBitsFor returns the difficulty the next block of a given lane has to have on top of the
// current tip. Both a miner and a pool need it when assembling a template.
func (hc *HeaderChain) NextBitsFor(lane mh.Lane) (uint32, error) {
	hc.mu.RLock()
	defer hc.mu.RUnlock()
	return hc.nextBitsLocked(hc.tip, lane)
}

// MinTime returns the earliest time the next block may carry.
func (hc *HeaderChain) MinTime() uint32 {
	hc.mu.RLock()
	defer hc.mu.RUnlock()
	return hc.minTimeLocked(hc.tip)
}

// minTimeLocked is the earliest time a block on top of `parent` may carry.
//
// From ForkLaneDifficulty on it is the parent's time: a block is never dated before the one it
// builds on. Before the fork the bound was the median of the last eleven blocks, which trails the
// tip by about six of them — a miner could date a block twelve minutes back, the block after it
// then looked twelve minutes late, and the lane's difficulty fell. A third of a lane doing that
// ran the lane 40 % faster than its hashrate allowed (cmd/powm-diffsim, scenario "backdate").
//
// The same time as the parent's is allowed. A strict bound would push a chain mined faster than
// one block a second (regtest) into the future by a second per block, and past FutureDrift.
func (hc *HeaderChain) minTimeLocked(parent *node) uint32 {
	if hc.params.Active(ForkLaneDifficulty, parent.height+1) {
		return parent.header.Time
	}
	return hc.medianTimePastLocked(parent) + 1
}

// AllowedLanesNow returns the lanes a block may be built for right now (the lane rotation rule).
func (hc *HeaderChain) AllowedLanesNow(now time.Time) []mh.Lane {
	hc.mu.RLock()
	recent := hc.recentLanesLocked(hc.tip, MaxSameLaneRun)
	tipTime := hc.tip.header.Time
	hc.mu.RUnlock()
	return AllowedLanes(recent, now, time.Unix(int64(tipTime), 0))
}

// Locator returns a chain of IDs from the tip backwards with exponentially growing steps — a peer
// works out from it where to start sending us headers.
func (hc *HeaderChain) Locator() [][32]byte {
	hc.mu.RLock()
	defer hc.mu.RUnlock()

	var out [][32]byte
	step := uint64(1)
	n := hc.tip
	for n != nil {
		out = append(out, n.id)
		if n.id == hc.genesis || len(out) >= 32 {
			break
		}
		for i := uint64(0); i < step && n != nil && n.id != hc.genesis; i++ {
			n = hc.byID[n.header.PrevBlock]
		}
		if len(out) >= 10 {
			step *= 2
		}
	}
	if len(out) == 0 || out[len(out)-1] != hc.genesis {
		out = append(out, hc.genesis)
	}
	return out
}

// HeadersFrom returns the active branch's headers after the locator, at most max of them.
func (hc *HeaderChain) HeadersFrom(locator [][32]byte, max int) []*Header {
	hc.mu.RLock()
	defer hc.mu.RUnlock()

	// Find the first ID from the locator that we have on the active branch.
	active := make(map[[32]byte]uint64)
	for n := hc.tip; n != nil; n = hc.byID[n.header.PrevBlock] {
		active[n.id] = n.height
		if n.id == hc.genesis {
			break
		}
	}
	start := hc.byID[hc.genesis].height
	for _, id := range locator {
		if h, ok := active[id]; ok {
			start = h
			break
		}
	}

	// Assemble the branch from the tip backwards and return the part above start.
	var chainUp []*node
	for n := hc.tip; n != nil; n = hc.byID[n.header.PrevBlock] {
		if n.height <= start {
			break
		}
		chainUp = append(chainUp, n)
		if n.id == hc.genesis {
			break
		}
	}
	out := make([]*Header, 0, len(chainUp))
	for i := len(chainUp) - 1; i >= 0 && len(out) < max; i-- {
		out = append(out, chainUp[i].header)
	}
	return out
}

// LoadHeader files a header **without verifying PoW**. It is meant exclusively for restoring from
// our own storage at node startup.
//
// Why PoW is not verified: the header was verified when the node accepted it from the network.
// Recomputing the PoW of a year of chain at every startup would take minutes (lane C is RandomX)
// and would protect against nothing — whoever can write into our datadir already owns the node.
// Bitcoin solves it the same way.
//
// **Call only from a restore.** Headers from the network go through AddHeader, which verifies PoW.
//
// The other checks (linkage, height, chainwork) run here too — corrupted storage shows up as an
// error at startup rather than as silent nonsense.
func (hc *HeaderChain) LoadHeader(h *Header) error {
	hc.mu.Lock()
	defer hc.mu.Unlock()

	id := h.ID()
	if _, dup := hc.byID[id]; dup {
		return ErrDuplicate
	}
	parent, ok := hc.byID[h.PrevBlock]
	if !ok {
		return ErrUnknownParent
	}
	if uint64(h.Height) != parent.height+1 {
		return ErrBadHeight
	}
	if hc.invalid[id] || parent.tainted {
		return ErrInvalidAncestor
	}
	// A checkpoint applies to a restore from disk as well. Otherwise a node that once downloaded
	// a fake branch would come up on it again after a restart — and a new rule in the binary would mean nothing.
	if cp, ok := hc.params.checkpointAt(uint64(h.Height)); ok && cp != id {
		return fmt.Errorf("%w at height %d", ErrCheckpoint, h.Height)
	}
	w, err := hc.normalizedWorkLocked(parent, h)
	if err != nil {
		return err
	}
	n := &node{header: h, id: id, work: new(big.Int).Add(parent.work, w), height: uint64(h.Height)}
	n.link(parent)
	hc.byID[id] = n
	// A restart has seen no votes, so of two siblings the lower ID is the tip again.
	if isSiblingLocked(n, hc.tip) {
		if hc.prefersSiblingLocked(n, hc.tip) {
			hc.tip = n
		}
	} else if n.work.Cmp(hc.tip.work) > 0 {
		hc.tip = n
	}
	return nil
}

// Lane statistics.
//
// This is the one thing no off-the-shelf explorer can show: bitcoin-style tools know one
// difficulty and one hashrate. Here every lane has its own retarget, so "the network difficulty"
// is not a single number — and **the share of blocks between the lanes** is the metric that shows
// whether CPU/GPU parity really works or one side is collapsing.
//
// The windows are Tari's 10/40/80/120 at a 120 s block, four times longer in blocks at 30 s so they
// cover the same time. Tari has the same problem with four algorithms: a short window shows the
// swing, a long one shows the trend, and the difference between them is the interesting part.

// LaneWindows are the windows the share of blocks is computed over.
var LaneWindows = []int{40, 160, 320, 480}

// LaneStat is the state of one lane.
type LaneStat struct {
	Lane mh.Lane
	// Bits is the difficulty the next block of this lane would get.
	Bits uint32
	// WorkPerBlock is the work corresponding to that difficulty.
	WorkPerBlock *big.Int
	// BlocksInWindow is the number of this lane's blocks in each window (index = LaneWindows).
	BlocksInWindow []int
	// LastHeight and LastTime are this lane's last block; LastHeight is zero when there is none.
	LastHeight uint64
	LastTime   uint32
	// AvgSpacingSec is the average spacing of **this lane's** blocks — computed from its own
	// blocks, not from the window length. It is compared with LaneBlockTime: the deviation says
	// whether the lane is keeping up.
	AvgSpacingSec float64
	// HashrateEst is an estimate of the lane's hashrate: the work of its blocks divided by the time they took.
	HashrateEst *big.Int
}

// LaneStats is a view of all the lanes at once.
type LaneStats struct {
	TipHeight uint64
	Windows   []int
	// WindowFill is how many blocks are really in the window. A young chain does not have 120
	// blocks, and a share computed against the nominal window size would then lie (9 of 120 instead of 9 of 25).
	WindowFill []int
	Lanes      []LaneStat
	// RunLane and RunLength are the lane at the tip and how many of its blocks run in a row.
	// Consensus allows at most MaxSameLaneRun; a higher value means a bug.
	RunLane   mh.Lane
	RunLength int
}

// HashratePoint is an estimate of a lane's hashrate at one moment.
type HashratePoint struct {
	// Time is the time of the block the window ends at.
	Time uint32
	// Hashrate is the window's work divided by its duration — hashes per second.
	Hashrate *big.Int
	// Height is the block the window ends at — the point in the graph refers to it.
	Height uint64
}

// HashrateSeries is the hashrate over time per lane (for the graph in the explorer).
type HashrateSeries struct {
	// WindowBlocks is how many of a lane's blocks go into one point.
	WindowBlocks int
	Lanes        map[mh.Lane][]HashratePoint
}

// HashrateSeries computes each lane's hashrate over time on the active branch: a sliding window of
// `window` blocks of the same lane (the window's work / the window's time), at most `points` points
// per lane out of the last `blocks` headers.
//
// It is computed **per lane**, not jointly: lane C and lane G are different algorithms and their
// hashrates do not add up (the same as in LaneStats). The genesis does not count — its time is a
// constant from the binary. Everything is integer; big.Int, because one block's work can be
// arbitrarily large.
func (hc *HeaderChain) HashrateSeries(blocks, window, points int) HashrateSeries {
	blocks = min(max(blocks, 2), 80_000)
	window = min(max(window, 2), 500)
	points = min(max(points, 2), 1_000)

	hc.mu.RLock()
	headers := make([]*Header, 0, blocks)
	for n := hc.tip; n != nil && len(headers) < blocks; n = hc.byID[n.header.PrevBlock] {
		if n.id == hc.genesis {
			break
		}
		headers = append(headers, n.header)
	}
	hc.mu.RUnlock()

	out := HashrateSeries{WindowBlocks: window, Lanes: map[mh.Lane][]HashratePoint{}}
	for _, lane := range mh.Lanes {
		// the lane's headers in chronological order (headers goes from the tip backwards)
		var times []uint32
		var works []*big.Int
		var heights []uint64
		for i := len(headers) - 1; i >= 0; i-- {
			h := headers[i]
			if h.Lane != lane {
				continue
			}
			w, err := Work(h.Bits)
			if err != nil {
				continue
			}
			times = append(times, h.Time)
			works = append(works, w)
			heights = append(heights, uint64(h.Height))
		}
		series := []HashratePoint{}
		if n := len(times); n >= 2 {
			w := min(window, n-1)
			sum := new(big.Int)
			for i := range w {
				sum.Add(sum, works[i+1]) // the work of the blocks in the window (the first block only sets the start time)
			}
			for i := w; i < n; i++ {
				if i > w {
					sum.Add(sum, works[i])
					sum.Sub(sum, works[i-w])
				}
				secs := int64(times[i]) - int64(times[i-w])
				if secs <= 0 {
					continue // time does not go backwards, but headers are not trusted (the MTP only bounds them)
				}
				series = append(series, HashratePoint{Time: times[i], Height: heights[i],
					Hashrate: new(big.Int).Div(sum, big.NewInt(secs))})
			}
		}
		out.Lanes[lane] = thinPoints(series, points)
	}
	return out
}

// thinPoints keeps at most `points` evenly spread points; the last point always stays.
func thinPoints(in []HashratePoint, points int) []HashratePoint {
	if len(in) <= points {
		return in
	}
	out := make([]HashratePoint, 0, points)
	for i := range points {
		out = append(out, in[i*(len(in)-1)/(points-1)])
	}
	return out
}

// LaneStats computes the lane statistics over the active branch.
//
// The genesis is **not counted**: its time is a constant written into the binary, not a mining
// time, so the spacing between it and the first block is an arbitrary number. Counted in, it gives
// an average spacing of millions of seconds against a target of 240 s — and a hashrate of zero.
func (hc *HeaderChain) LaneStats() LaneStats {
	hc.mu.RLock()
	defer hc.mu.RUnlock()

	maxWindow := LaneWindows[len(LaneWindows)-1]
	// Walk the window once and remember the headers; computing it separately for each lane would
	// mean walking the chain as many times as there are lanes.
	headers := make([]*Header, 0, maxWindow)
	for n := hc.tip; n != nil && len(headers) < maxWindow; n = hc.byID[n.header.PrevBlock] {
		if n.id == hc.genesis {
			break
		}
		headers = append(headers, n.header)
	}

	out := LaneStats{
		Windows:    append([]int(nil), LaneWindows...),
		WindowFill: make([]int, len(LaneWindows)),
	}
	if hc.tip != nil {
		out.TipHeight = hc.tip.height
	}
	for wi, size := range LaneWindows {
		out.WindowFill[wi] = min(size, len(headers))
	}
	if len(headers) > 0 {
		out.RunLane = headers[0].Lane
		for _, h := range headers {
			if h.Lane != out.RunLane {
				break
			}
			out.RunLength++
		}
	}

	for _, lane := range mh.Lanes {
		st := LaneStat{Lane: lane, BlocksInWindow: make([]int, len(LaneWindows))}
		if bits, err := hc.NextBitsForLocked(lane); err == nil {
			st.Bits = bits
			if w, err := Work(bits); err == nil {
				st.WorkPerBlock = w
			}
		}
		laneWork := new(big.Int)
		var count int
		var newest, oldest uint32
		for i, h := range headers {
			if h.Lane != lane {
				continue
			}
			if count == 0 {
				st.LastHeight, st.LastTime = uint64(h.Height), h.Time
				newest = h.Time
			}
			oldest = h.Time
			count++
			if w, err := Work(h.Bits); err == nil {
				laneWork.Add(laneWork, w)
			}
			for wi, size := range LaneWindows {
				if i < size {
					st.BlocksInWindow[wi]++
				}
			}
		}
		// Both the spacing and the hashrate are computed from **this lane's** times, not from the
		// window length. Otherwise a lane that is not mining right now would drag down the other one's average.
		if span := int64(newest) - int64(oldest); count > 1 && span > 0 {
			st.AvgSpacingSec = float64(span) / float64(count-1)
			st.HashrateEst = new(big.Int).Div(laneWork, big.NewInt(span))
		}
		out.Lanes = append(out.Lanes, st)
	}
	return out
}

// NextBitsForLocked is NextBitsFor without the lock — called from inside, where we already hold it.
func (hc *HeaderChain) NextBitsForLocked(lane mh.Lane) (uint32, error) {
	return hc.nextBitsLocked(hc.tip, lane)
}
