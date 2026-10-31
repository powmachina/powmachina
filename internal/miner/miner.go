// Package miner is the PowMachina miner: lane C on the CPU (MachinaHash-C = RandomX v2) and lane G on
// the GPU (MachinaHash-G through the libpowm-cuda plugin), both at once in one process.
//
// The work is supplied by a Feed: solo against your own node (NodeFeed — gettemplate, the miner
// assembles the coinbase itself, submitblock) or stratum (StratumFeed — a node with a stratum port or a
// pool). The miner's core only hashes. The CPU calls `internal/mh` directly; the GPU kernel is a
// bit-exact port and the miner verifies it against `internal/mh` at every epoch — if it differed, it
// turns the card off (project rules 1 and 3: one implementation, and the GPU must be bit-exact).
package miner

import (
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"math"
	"math/big"
	"math/rand/v2"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"powmachina/internal/chain"
	"powmachina/internal/mh"
	"powmachina/internal/work"
)

// GPU is a card the miner can use (gpu.Device implements it).
type GPU interface {
	SetEpoch(cache []uint32, datasetItems uint32, ops [16][3]uint8) error
	SetJob(header *[80]byte, target *[32]byte) error
	Search(start uint64) ([]uint64, uint64, error)
	Hash(header *[80]byte, nonce uint64) ([32]byte, error)
	Config() (threadsPerBlock, blocks int, perThread uint32)
	// Index is the card's device ordinal (the nvidia-smi index), for the logs. It is not the
	// position in the selection — with --gpu-devices 4 the one card is still device 4, not GPU 0.
	Index() int
}

// Feed supplies the work and submits the nonces found.
type Feed interface {
	// Run sends jobs through `set` (nil = there is no work for the lane right now) until quit is closed.
	Run(lanes []mh.Lane, set func(mh.Lane, *work.Job), quit <-chan struct{}) error
	// Submit sends a nonce found for a job. It returns whether the other side accepted it and whether it was a block.
	Submit(j *work.Job, nonce uint64) (accepted, block bool, err error)
}

// HashrateReporter is a Feed that can pass on how fast a lane is hashing (a stratum connection).
type HashrateReporter interface {
	ReportHashrate(lane mh.Lane, hps float64)
}

// Options are the miner's settings.
type Options struct {
	Params  chain.NetParams
	Threads int // CPU threads for lane C; 0 = DefaultThreads, <0 = the CPU does not mine
	// GPUs are the opened cards for lane G (empty = the GPU does not mine).
	GPUs []GPU
	// Light: RandomX without the 2 GiB dataset — orders of magnitude slower, only for weak machines and tests.
	Light bool
	// NoAffinity: do not pin the threads to cores.
	NoAffinity bool
	// LargePages: try large pages (on EPYC ~2× faster; vm.nr_hugepages is needed).
	LargePages bool
	// GPUDuty is the fraction of time the miner may keep the card busy (0 < duty ≤ 1; 0 = full speed).
	// It serves to share the card with other work — after every kernel launch it waits long enough for
	// the ratio of work to silence to match the given fraction.
	//
	// The card's memory is not freed by it: lane G's DAG has to stay loaded, otherwise it would be
	// rebuilt every round (seconds).
	GPUDuty float64
	Log     *log.Logger
}

// task is a job prepared for hashing (with the epoch's data for the GPU).
type task struct {
	job   *work.Job
	epoch *epochData
	// block is the block's own target, read from the header's bits: a job from a pool carries only the
	// share target, and the miner must never look for anything harder than a block.
	block [32]byte
	// base is where the devices start counting nonces for this task, drawn at random. A pool sends the
	// same header again whenever it changes the share target; started from zero each time, a miner
	// would search one stretch of nonces over and over, and a header that holds no block in that
	// stretch would never give one.
	base uint64
}

// newTask reads the block target out of the job's header. A header whose bits do not decode leaves it
// zero, which only means the target is never capped.
func newTask(j *work.Job) *task {
	t := &task{job: j, base: rand.Uint64() >> 16}
	if block, err := chain.CompactToTarget(binary.LittleEndian.Uint32(j.PoW[72:76])); err == nil {
		t.block = block
	}
	return t
}

// target is what the devices compare a hash with: the job's target halved `tighten` times, but never
// harder than a block. Every hash that meets it meets the job's target too, so the other side accepts
// it as it would any other — and every hash that is a block still meets it.
func (t *task) target(tighten uint32) [32]byte {
	if tighten == 0 || !mh.MeetsTarget(&t.block, &t.job.Target) {
		return t.job.Target
	}
	n := new(big.Int).SetBytes(t.job.Target[:])
	n.Rsh(n, uint(tighten))
	if n.Cmp(new(big.Int).SetBytes(t.block[:])) < 0 {
		return t.block
	}
	var out [32]byte
	n.FillBytes(out[:])
	return out
}

// epochData is what the GPU needs to build the dataset, plus a reference verifier for checking.
type epochData struct {
	epoch  uint64
	cache  []uint32
	items  uint32
	ops    [16][3]uint8
	verify *mh.Verifier // light — only to check the card
}

// Miner mines lane C on the CPU and lane G on the GPU.
type Miner struct {
	opt   Options
	found chan found
	lanes map[mh.Lane]*lane

	mu       sync.Mutex // preparing epochs (setJob is called by the feed)
	rx       *mh.RandomX
	rxEpoch  uint64
	stop     chan struct{}
	wg       sync.WaitGroup
	gpuStop  chan struct{}
	gpuWG    sync.WaitGroup
	gpuFatal chan error // a card that has given up; it stops the whole miner (nil = no cards)
	epochG   *epochData

	// Accepted/Rejected: nonces submitted (blocks when solo, shares with stratum). Blocks: blocks.
	Accepted, Rejected, Blocks atomic.Uint64
	// DeviceHashes: the hash count per device (index 0 = the CPU, 1… = the GPUs in order).
	DeviceHashes []atomic.Uint64
}

type lane struct {
	id  mh.Lane
	job atomic.Pointer[task]

	// tighten is how many times the lane halves the target it was given (see retune).
	tighten atomic.Uint32

	// the submission pace (the main Run loop only)
	lastTask *task
	tokens   float64
	tokensAt time.Time
	seen     int // solutions the devices handed over since seenAt
	seenAt   time.Time

	// the speed while there is work (the main Run loop only; see measure)
	lastHashes  uint64
	lastAt      time.Time
	hadWork     bool
	workHashes  uint64
	workSeconds float64
	reportedAt  time.Time
}

// measure adds up the hashes a lane did while it had a job and, every 30 s of the clock, returns
// the rate over that time. A lane the rotation rule keeps asleep for most of a minute still hashes
// at full speed in the rest of it — averaged over the whole minute it would look several times
// slower than the hardware is. Seconds without a job are therefore left out, and a stretch with
// under five seconds of work says nothing and is carried over.
func (l *lane) measure(now time.Time, hashes uint64) (float64, bool) {
	hasWork := l.job.Load() != nil
	if !l.lastAt.IsZero() && hasWork && l.hadWork && hashes >= l.lastHashes {
		l.workHashes += hashes - l.lastHashes
		l.workSeconds += now.Sub(l.lastAt).Seconds()
	}
	l.lastHashes, l.lastAt, l.hadWork = hashes, now, hasWork
	if l.reportedAt.IsZero() {
		l.reportedAt = now
	}
	if now.Sub(l.reportedAt) < 30*time.Second || l.workSeconds < 5 {
		return 0, false
	}
	rate := float64(l.workHashes) / l.workSeconds
	l.workHashes, l.workSeconds, l.reportedAt = 0, 0, now
	return rate, true
}

// The pace: what a lane submits per second at most, what it aims for when it has to tighten, and below
// what it loosens again.
const (
	paceLimit  = 2.0
	paceAim    = 1.0
	paceLoosen = 0.25
)

// retune keeps the devices from finding more than the lane may submit.
//
// A target that yields dozens of solutions a second (a pool that starts every connection at a low
// difficulty, or one that never raises it) cannot be handled by dropping whatever comes after the
// second one. The miner cannot tell a share from a block — the pool can — so the block would go the
// same way as the rest: at 47 solutions a second that is 96 % of the blocks found.
//
// So the lane looks for a harder target instead. What it stops finding are the worst hashes; what is
// left is the best of them, and a block is always among those. It returns the new number of halvings
// and whether it changed.
func (l *lane) retune(now time.Time) (uint32, bool) {
	cur := l.tighten.Load()
	dt := now.Sub(l.seenAt).Seconds()
	if l.seenAt.IsZero() || dt < 0 {
		l.seen, l.seenAt = 0, now
		return cur, false
	}
	if dt < 2 {
		return cur, false
	}
	rate := float64(l.seen) / dt
	next := cur
	switch {
	case rate > paceLimit:
		next = min(cur+uint32(math.Ceil(math.Log2(rate/paceAim))), 64)
	case dt < 10:
		return cur, false // too early to call it quiet
	case rate < paceLoosen && cur > 0:
		next = cur - 1
	}
	l.seen, l.seenAt = 0, now
	if next == cur {
		return cur, false
	}
	l.tighten.Store(next)
	return next, true
}

func (l *lane) allow(t *task) bool {
	now := time.Now()
	if t != l.lastTask {
		l.lastTask = t
		return true
	}
	l.tokens = min(4, l.tokens+now.Sub(l.tokensAt).Seconds()*2)
	l.tokensAt = now
	if l.tokens < 1 {
		return false
	}
	l.tokens--
	return true
}

type found struct {
	t     *task
	nonce uint64
	// block: the device saw that the hash meets the block's target (the CPU has the hash in hand; a
	// card returns nonces only, so for lane G this stays false).
	block bool
}

// DefaultThreads is a sensible number of lane C threads: RandomX needs ~2 MiB of L3 per thread, so more
// threads than L3/2 MiB only fight over the cache (typically an 8-core with 16 MB of L3: 8, not 16).
func DefaultThreads() int {
	n := runtime.NumCPU()
	if l3 := L3Bytes(); l3 > 0 {
		if byCache := int(l3 / (2 << 20)); byCache >= 1 && byCache < n {
			n = byCache
		}
	}
	return n
}

// New creates the miner.
func New(opt Options) *Miner {
	if opt.Threads == 0 {
		opt.Threads = DefaultThreads()
	}
	if opt.Log == nil {
		opt.Log = log.Default()
	}
	m := &Miner{opt: opt, found: make(chan found, 256), lanes: map[mh.Lane]*lane{},
		DeviceHashes: make([]atomic.Uint64, 1+len(opt.GPUs))}
	if opt.Threads > 0 {
		m.lanes[mh.LaneC] = &lane{id: mh.LaneC}
	}
	if len(opt.GPUs) > 0 {
		m.lanes[mh.LaneG] = &lane{id: mh.LaneG}
	}
	return m
}

// Lanes are the lanes the miner has devices for.
func (m *Miner) Lanes() []mh.Lane {
	var out []mh.Lane
	for _, id := range []mh.Lane{mh.LaneC, mh.LaneG} {
		if m.lanes[id] != nil {
			out = append(out, id)
		}
	}
	return out
}

// laneHashes is the number of hashes a lane's devices have done: the CPU for lane C, the cards for G.
func (m *Miner) laneHashes(id mh.Lane) uint64 {
	if id == mh.LaneC {
		return m.DeviceHashes[0].Load()
	}
	var n uint64
	for i := 1; i < len(m.DeviceHashes); i++ {
		n += m.DeviceHashes[i].Load()
	}
	return n
}

// Hashes is the number of hashes since the start (every device).
func (m *Miner) Hashes() uint64 {
	var n uint64
	for i := range m.DeviceHashes {
		n += m.DeviceHashes[i].Load()
	}
	return n
}

// Run mines with the work from `feed` until `quit` is closed.
func (m *Miner) Run(feed Feed, quit <-chan struct{}) error {
	if len(m.lanes) == 0 {
		return errors.New("miner: no CPU threads and no GPU — nothing to mine with")
	}
	defer m.close()
	if len(m.opt.GPUs) > 0 {
		m.startGPUs()
	}
	feedErr := make(chan error, 1)
	go func() { feedErr <- feed.Run(m.Lanes(), m.setJob, quit) }()
	tune := time.NewTicker(time.Second)
	defer tune.Stop()
	reporter, _ := feed.(HashrateReporter)
	for {
		select {
		case now := <-tune.C:
			for _, l := range m.lanes {
				if n, changed := l.retune(now); changed {
					m.opt.Log.Printf("lane %s: looking for a target %d× harder than asked, so that nothing found has to be dropped", l.id, uint64(1)<<min(n, 63))
				}
				if rate, ok := l.measure(now, m.laneHashes(l.id)); ok && reporter != nil {
					go reporter.ReportHashrate(l.id, rate) // a slow connection must not hold up the loop
				}
			}
		case <-quit:
			// Wait for the feed to finish. Otherwise `defer m.close()` runs alongside `setJob` from
			// the feed and overwrites its RandomX under its hands (found with -race).
			<-feedErr
			return nil
		case err := <-feedErr:
			return err
		case err := <-m.gpuFatal:
			// A card is stuck and cannot recover inside the process; stop so a supervisor restarts
			// the miner with a clean GPU state. nil when there are no cards — that case never fires.
			return err
		case f := <-m.found:
			l := m.lanes[f.t.job.Lane]
			if l == nil || l.job.Load() != f.t {
				continue // the job ended in the meantime
			}
			l.seen++
			// The first solution of a new job always goes (it may be a block), and so does one the
			// device knows to be a block; further ones from the same job at most 2 per second per lane
			// — at a low difficulty a card finds dozens of them per launch and the server would refuse
			// them anyway. This is only the stopgap for the seconds until retune has tightened the
			// target: past that the devices do not find more than the lane sends.
			if !l.allow(f.t) && !f.block {
				continue
			}
			ok, block, err := feed.Submit(f.t.job, f.nonce)
			switch {
			case err != nil:
				m.Rejected.Add(1)
				m.opt.Log.Printf("lane %s: submit failed: %v", f.t.job.Lane, err)
			case !ok:
				m.Rejected.Add(1)
			default:
				m.Accepted.Add(1)
				if block {
					m.Blocks.Add(1)
				}
			}
		}
	}
}

func (m *Miner) close() {
	// Stop the threads and the cards first — they take the lock themselves, so we must not hold it here.
	m.stopWorkers()
	m.stopGPUs()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.rx != nil {
		m.rx.Close()
		m.rx = nil
	}
}

// setJob prepares a job (the RandomX epoch for the CPU, the cache for the GPU) and gives it to the devices.
func (m *Miner) setJob(id mh.Lane, j *work.Job) {
	l := m.lanes[id]
	if l == nil {
		return
	}
	if j == nil {
		l.job.Store(nil)
		return
	}
	// The epoch comes with the job, from whoever sent it, and everything below is built for it.
	if !mh.ValidEpoch(j.Epoch) {
		m.opt.Log.Printf("lane %s: a job for epoch %d, which no chain can reach — ignored", id, j.Epoch)
		l.job.Store(nil)
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	t := newTask(j)
	if id == mh.LaneG {
		if m.epochG == nil || m.epochG.epoch != j.Epoch {
			m.opt.Log.Printf("lane G: preparing epoch %d cache…", j.Epoch)
			m.epochG = newEpochData(j.Epoch, m.opt.Params.DagProfile)
		}
		t.epoch = m.epochG
		l.job.Store(t)
		return
	}
	if m.rx == nil || m.rxEpoch != j.Epoch {
		l.job.Store(nil)
		m.stopWorkers()
		if m.rx != nil {
			m.rx.Close()
			m.rx = nil
		}
		mode := "fast (2 GiB dataset)"
		if m.opt.Light {
			mode = "light"
		}
		m.opt.Log.Printf("lane C: preparing RandomX for epoch %d, %s…", j.Epoch, mode)
		start := time.Now()
		rx, err := mh.NewRandomXForMiner(j.Epoch, !m.opt.Light, false, m.opt.LargePages)
		if err != nil {
			m.opt.Log.Printf("lane C: RandomX: %v — CPU mining paused", err)
			return
		}
		m.rx, m.rxEpoch = rx, j.Epoch
		m.opt.Log.Printf("lane C: RandomX ready in %v (large pages: %v), %d threads",
			time.Since(start).Round(time.Millisecond), rx.UsesLargePages(), m.opt.Threads)
		if m.opt.LargePages && !rx.UsesLargePages() && !m.opt.Light {
			m.opt.Log.Printf("lane C: tip — large pages make RandomX up to 2× faster: sudo sysctl -w vm.nr_hugepages=%d", 1280+64*m.opt.Threads)
		}
		l.job.Store(t)
		m.startWorkers()
		return
	}
	l.job.Store(t)
}

func newEpochData(epoch uint64, profile mh.DagProfile) *epochData {
	p := profile.Params(epoch)
	c := mh.BuildCache(p)
	return &epochData{epoch: epoch, cache: c.Words(), items: uint32(p.DatasetItems()),
		ops: mh.NewRandMath(epoch).OpsRaw(), verify: mh.NewLightVerifierFrom(epoch, c)}
}

func (m *Miner) startWorkers() {
	m.stop = make(chan struct{})
	order := CoreOrder()
	for i := range m.opt.Threads {
		h, err := mh.NewHasherWithRandomX(m.rx)
		if err != nil {
			m.opt.Log.Printf("thread %d: %v", i, err)
			continue
		}
		cpu := -1
		if !m.opt.NoAffinity && len(order) > 0 {
			cpu = order[i%len(order)]
		}
		m.wg.Add(1)
		go m.work(uint64(i), uint64(m.opt.Threads), h, cpu)
	}
}

func (m *Miner) stopWorkers() {
	if m.stop == nil {
		return
	}
	close(m.stop)
	m.wg.Wait()
	m.stop = nil
}

func (m *Miner) send(t *task, nonce uint64, block bool) {
	if block && !mh.MeetsTarget(&t.block, &t.job.Target) {
		block = false // a job whose target is already the block's: every solution is one, none is special
	}
	if block {
		select { // a block waits for its turn, it is never dropped
		case m.found <- found{t, nonce, true}:
		case <-m.stop:
		}
		return
	}
	select {
	case m.found <- found{t, nonce, false}:
	default: // the queue is full — the share is dropped, the miner does not wait
	}
}

// work hashes the nonces i, i+n, i+2n… of lane C's current job. It checks the job every `batch` hashes
// — at ~1–2 ms per hash per thread that is ~0.3 s of delay after a new block.
//
// The hashing is pipelined (RandomX calculate_hash_first/next): while the hash of nonce n is finishing,
// RandomX is already preparing n+stride. The thread is pinned to a core (cpu ≥ 0).
func (m *Miner) work(start, stride uint64, h *mh.Hasher, cpu int) {
	defer m.wg.Done()
	defer h.Close()
	if cpu >= 0 {
		pin(cpu)
		defer runtime.UnlockOSThread()
	}
	const batch = 256
	l := m.lanes[mh.LaneC]
	for {
		select {
		case <-m.stop:
			return
		default:
		}
		t := l.job.Load()
		if t == nil {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		hdr := t.job.PoW
		pipe, err := h.PipeLaneC(&hdr)
		if err != nil {
			m.opt.Log.Printf("hash: %v", err)
			return
		}
		nonce := start + t.base/stride*stride // a multiple of the stride keeps the threads apart
		pipe.Start(nonce)
		var out [32]byte
		for l.job.Load() == t {
			target := t.target(l.tighten.Load())
			for range batch {
				pipe.Next(nonce+stride, &out) // the hash of `nonce`, starts the next one
				if mh.MeetsTarget(&out, &target) {
					m.send(t, nonce, mh.MeetsTarget(&out, &t.block))
				}
				nonce += stride
			}
			m.DeviceHashes[0].Add(batch)
			select {
			case <-m.stop:
				return
			default:
			}
		}
	}
}

func (m *Miner) startGPUs() {
	m.gpuStop = make(chan struct{})
	// Buffered by the number of cards: a card sends here at most once and then returns, so no
	// send can block even if Run has already stopped reading.
	m.gpuFatal = make(chan error, len(m.opt.GPUs))
	for i, g := range m.opt.GPUs {
		m.gpuWG.Add(1)
		go m.gpuWork(i, g)
	}
}

func (m *Miner) stopGPUs() {
	if m.gpuStop == nil {
		return
	}
	close(m.gpuStop)
	m.gpuWG.Wait()
	m.gpuStop = nil
}

// gpuEpochRetry is how long a card waits before it tries again an epoch it could not be set up for.
// A card that was short of memory for a moment comes back by itself; one that computes wrong hashes
// fails the check again and stays idle.
var gpuEpochRetry = time.Minute

// gpuMaxConsecFail is how many CUDA errors in a row on one card (with no launch succeeding in
// between) count as a stuck lane G. A single failed launch recovers and the count resets; a sticky
// fault — an illegal address under WDDM, say — makes every later CUDA call fail, and that cannot be
// cleared inside the process: the device would have to be reset process-wide, taking the other cards
// with it. So past this many in a row the miner stops with a non-zero exit and lets a supervisor
// restart it with a clean GPU state, instead of logging the same error every second at 0 H/s while
// lane C mines on and nothing notices. A var so a test can lower it.
var gpuMaxConsecFail = 30

// gpuGaveUp records that a card has failed gpuMaxConsecFail times in a row and, at the threshold,
// tells Run to stop the whole miner. It returns true when the caller should give up on the card.
func (m *Miner) gpuGaveUp(dev, n int, err error) bool {
	if n < gpuMaxConsecFail {
		return false
	}
	select {
	case m.gpuFatal <- fmt.Errorf("GPU %d: %d CUDA failures in a row (last: %v) — lane G is stuck and cannot recover in-process; exiting so a supervisor can restart the miner", dev, n, err):
	default:
	}
	return true
}

// gpuWork serves one card: on a new epoch it builds the dataset and verifies the card against
// internal/mh, on a new job it uploads the header and searches. The cards split the nonces by their top bits.
func (m *Miner) gpuWork(idx int, g GPU) {
	defer m.gpuWG.Done()
	l := m.lanes[mh.LaneG]
	dev := g.Index() // the device ordinal, for the logs; idx is the position in the selection
	var epoch *epochData
	var cur *task
	var curTighten uint32
	var nonce uint64
	var consecFail int // CUDA errors in a row (reset by any successful launch)
	// failed is the epoch the card could not be set up for, and failedAt when.
	var failed *epochData
	var failedAt time.Time
	for {
		select {
		case <-m.gpuStop:
			return
		default:
		}
		t := l.job.Load()
		if t == nil {
			time.Sleep(50 * time.Millisecond)
			continue
		}
		if t.epoch != epoch {
			if t.epoch == failed && time.Since(failedAt) < gpuEpochRetry {
				time.Sleep(50 * time.Millisecond)
				continue
			}
			start := time.Now()
			err := g.SetEpoch(t.epoch.cache, t.epoch.items, t.epoch.ops)
			if err == nil {
				err = verifyGPU(g, t.job, t.epoch.verify)
			}
			if err != nil {
				// The card sits this epoch out, it is not given up on. The epoch comes with the job:
				// one job for an epoch whose dataset does not fit the card would otherwise switch the
				// card off until the miner is restarted, with the CPU mining on as if all were well.
				// Whatever the card has loaded now is not known to be right, so it starts over.
				m.opt.Log.Printf("GPU %d: %v — the card does not mine epoch %d; it tries again with another epoch or in %v",
					dev, err, t.epoch.epoch, gpuEpochRetry)
				failed, failedAt, epoch, cur = t.epoch, time.Now(), nil, nil
				continue
			}
			failed = nil
			tpb, blocks, pt := g.Config()
			m.opt.Log.Printf("GPU %d: epoch %d DAG built and verified against the reference in %v (grid %d×%d, %d per thread)",
				dev, t.epoch.epoch, time.Since(start).Round(time.Millisecond), blocks, tpb, pt)
			epoch, cur = t.epoch, nil
		}
		if tighten := l.tighten.Load(); t != cur || tighten != curTighten {
			hdr, target := t.job.PoW, t.target(tighten)
			if err := g.SetJob(&hdr, &target); err != nil {
				consecFail++
				m.opt.Log.Printf("GPU %d: %v", dev, err)
				if m.gpuGaveUp(dev, consecFail, err) {
					return
				}
				time.Sleep(time.Second)
				continue
			}
			if t != cur {
				nonce = uint64(idx)<<56 + t.base // a new target on the same job carries on where it was
			}
			cur, curTighten = t, tighten
		}
		started := time.Now()
		nonces, hashes, err := g.Search(nonce)
		if err != nil {
			consecFail++
			m.opt.Log.Printf("GPU %d: %v", dev, err)
			if m.gpuGaveUp(dev, consecFail, err) {
				return
			}
			time.Sleep(time.Second)
			continue
		}
		consecFail = 0
		if pause := dutyPause(time.Since(started), m.opt.GPUDuty); pause > 0 {
			select {
			case <-m.gpuStop:
				return
			case <-time.After(pause):
			}
		}
		nonce += hashes
		m.DeviceHashes[1+idx].Add(hashes)
		for _, n := range nonces {
			m.send(cur, n, false)
		}
	}
}

// dutyPause returns how long the miner should wait after a kernel launch so that it keeps the card busy
// only for the given fraction of the time.
//
// With a duty of 0.2 and a kernel 10 ms long it waits 40 ms — the card is then free for other work 80 %
// of the time. It is the only way to share a card: a kernel cannot be "slowed down", it can only be
// launched less often.
func dutyPause(work time.Duration, duty float64) time.Duration {
	if duty <= 0 || duty >= 1 || work <= 0 {
		return 0
	}
	return time.Duration(float64(work) * (1 - duty) / duty)
}

// verifyGPU compares the card's hash with the reference from internal/mh. A difference of one bit =
// blocks the network will refuse; such a card must not mine.
func verifyGPU(g GPU, j *work.Job, v *mh.Verifier) error {
	h := mh.NewHasher()
	defer h.Close()
	for _, nonce := range []uint64{0, 1, 0xdeadbeefcafe} {
		hdr := j.PoW
		got, err := g.Hash(&hdr, nonce)
		if err != nil {
			return err
		}
		want, err := h.Hash(v, &hdr, nonce, mh.LaneG)
		if err != nil {
			return err
		}
		if got != want {
			return fmt.Errorf("GPU hash differs from the reference for nonce %d (GPU %x, reference %x) — driver or build problem", nonce, got[:8], want[:8])
		}
	}
	return nil
}

// Benchmark hashes for `d` on a fake job (the target is never met) and returns the hash count per device
// (0 = the CPU, 1… = the GPUs). Without a node — for comparing machines and settings.
func Benchmark(opt Options, d time.Duration) ([]uint64, error) {
	m := New(opt)
	defer m.close()
	if len(opt.GPUs) > 0 {
		m.startGPUs()
	}
	for _, id := range m.Lanes() {
		j := &work.Job{Lane: id} // a zero target — nothing meets it
		j.PoW[0] = 1
		m.setJob(id, j)
	}
	active := func(i int) bool { return (i == 0 && m.rx != nil) || (i > 0 && m.lanes[mh.LaneG] != nil) }
	deadline := time.Now().Add(2 * time.Minute)
	for {
		ready := true
		for i := range m.DeviceHashes {
			if active(i) && m.DeviceHashes[i].Load() == 0 {
				ready = false
			}
		}
		if ready || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	before := make([]uint64, len(m.DeviceHashes))
	for i := range before {
		before[i] = m.DeviceHashes[i].Load()
	}
	time.Sleep(d)
	out := make([]uint64, len(m.DeviceHashes))
	for i := range out {
		out[i] = m.DeviceHashes[i].Load() - before[i]
	}
	return out, nil
}
