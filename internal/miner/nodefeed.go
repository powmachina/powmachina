package miner

import (
	"errors"
	"log"
	"math/big"
	"strings"
	"sync"
	"time"

	"powmachina/internal/chain"
	"powmachina/internal/mh"
	"powmachina/internal/nodeclient"
	"powmachina/internal/work"
)

// Source is the node mined against when solo. nodeclient.Client implements it.
type Source interface {
	Template(lane string) (*nodeclient.Template, error)
	SubmitBlock(b *chain.Block) error
	// SubmitShare sends a share and returns whether it is a block too.
	SubmitShare(s *chain.Share) (bool, error)
	Tip() (uint64, [32]byte, error)
}

// NodeFeed is solo mining against your own node: the template through gettemplate, the coinbase to your
// own address (work.BuildShares), every share through submitshare and the block through submitblock.
// A node that takes no shares (a light one) gets the block alone.
type NodeFeed struct {
	Src     Source
	Payout  []byte
	Params  chain.NetParams
	Refresh time.Duration // a new template even without a new block (new transactions, the time); 0 = 20 s
	Poll    time.Duration // how often to ask for a new tip; 0 = 1 s
	Retry   time.Duration // the pause before a block that did not get through is sent again; 0 = 500 ms
	Log     *log.Logger

	mu sync.Mutex
	// done are the jobs the node has answered for, and whether it took the block. A job whose block
	// did not reach the node is not among them.
	done   map[*work.Job]bool
	kick   chan struct{}
	logged map[mh.Lane]bool
	// rate is each lane's hashrate as the miner measured it (ReportHashrate), weight the share
	// weight its jobs were last built with.
	rate   map[mh.Lane]float64
	weight map[mh.Lane]uint8
}

// shareRate is how many shares a second a lane aims to submit at most. The weight goes up until it
// does: a share of weight k counts as 2^k, so the miner gets the same for its work, and its node
// and the network verify fewer of them.
const shareRate = 0.5

// shareWeight is the lightest weight at which `hps` hashes a second find at most shareRate shares
// on a block of `bits`. An unknown rate gets the heaviest weight.
func shareWeight(hps float64, bits uint32) uint8 {
	work, err := chain.Work(bits)
	if hps <= 0 || err != nil {
		return chain.MaxShareWeight
	}
	hashes, _ := new(big.Float).SetInt(work).Float64() // hashes per block
	rate := hps * chain.SharesPerLaneBlock / hashes    // shares a second at weight 0
	k := uint8(0)
	for k < chain.MaxShareWeight && rate > shareRate {
		rate /= 2
		k++
	}
	return k
}

// ReportHashrate implements miner.HashrateReporter: the next jobs of the lane are built for it.
func (f *NodeFeed) ReportHashrate(lane mh.Lane, hps float64) {
	f.init()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rate[lane] = hps
}

func (f *NodeFeed) init() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.done == nil {
		f.done = map[*work.Job]bool{}
		f.kick = make(chan struct{}, 1)
		f.logged = map[mh.Lane]bool{}
		f.rate = map[mh.Lane]float64{}
		f.weight = map[mh.Lane]uint8{}
	}
	if f.Refresh <= 0 {
		f.Refresh = 20 * time.Second
	}
	if f.Poll <= 0 {
		f.Poll = time.Second
	}
	if f.Retry <= 0 {
		f.Retry = 500 * time.Millisecond
	}
	if f.Log == nil {
		f.Log = log.Default()
	}
}

// Run implements Feed.
func (f *NodeFeed) Run(lanes []mh.Lane, set func(mh.Lane, *work.Job), quit <-chan struct{}) error {
	f.init()
	type state struct {
		tip  [32]byte
		at   time.Time
		have bool
	}
	st := map[mh.Lane]*state{}
	for _, l := range lanes {
		st[l] = &state{}
	}
	poll := time.NewTicker(f.Poll)
	defer poll.Stop()
	for {
		_, tip, err := f.Src.Tip()
		if err != nil {
			f.Log.Printf("node not reachable: %v", err)
		}
		for _, l := range lanes {
			s := st[l]
			if err != nil || (s.have && s.tip == tip && time.Since(s.at) < f.Refresh) {
				continue
			}
			j, jerr := f.job(l)
			f.mu.Lock()
			blocked := f.logged[l]
			f.mu.Unlock()
			if jerr != nil && s.have && tipNotApplied(jerr) {
				// The node has the new tip's header, not yet its body: the job on the block below
				// goes on until there is one on the new tip (s.tip stays, so the next poll asks again).
				continue
			}
			if jerr != nil {
				if !blocked {
					f.Log.Printf("lane %s: no work: %v — waiting", l, jerr)
				}
				f.mu.Lock()
				f.logged[l] = true
				f.mu.Unlock()
				s.have = false
				set(l, nil)
				continue
			}
			if blocked {
				f.Log.Printf("lane %s: mining again", l)
			}
			f.mu.Lock()
			f.logged[l] = false
			f.mu.Unlock()
			s.tip, s.at, s.have = tip, time.Now(), true
			set(l, j)
		}
		select {
		case <-quit:
			return nil
		case <-f.kick:
			for _, s := range st {
				s.have = false // after our own block, new templates right away
			}
		case <-poll.C:
		}
	}
}

// tipNotApplied: the node's answer that its new tip has no body applied yet. Over RPC it comes as
// the node's text.
func tipNotApplied(err error) bool {
	return errors.Is(err, work.ErrTipNotApplied) || strings.Contains(err.Error(), work.ErrTipNotApplied.Error())
}

func (f *NodeFeed) job(l mh.Lane) (*work.Job, error) {
	t, err := f.Src.Template(l.String())
	if err != nil {
		return nil, err
	}
	bits, err := work.ParseBits(t.Bits)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	weight := shareWeight(f.rate[l], bits)
	last, had := f.weight[l]
	f.weight[l] = weight
	f.mu.Unlock()
	j, err := work.BuildShares(t, f.Payout, weight, f.Params)
	if err == nil && j.Shares && (!had || last != weight) {
		f.Log.Printf("lane %s: shares of weight %d (%d units each)", l, weight, uint64(1)<<weight)
	}
	return j, err
}

// How hard a block is pushed at a node that does not answer: this many more times, and not for
// longer than this. Past that the chain has most likely moved on and the block is worth nothing.
const (
	submitRetries = 3
	submitWindow  = 10 * time.Second
)

// Submit implements Feed: it sends the block of a job to the node. A job the node has answered for
// does not send further nonces — it would refuse them as duplicates, or for the reason it refused
// the first.
//
// A block is the whole reward, so one that did not get through is not let go at the first error. An
// answer from the node settles the job either way. No answer at all (a timeout, a connection reset,
// the node restarting) settles nothing: the block is sent again, and if it still does not get
// through, the job stays open and the next nonce found for it is sent as well.
//
// With shares, every nonce goes to submitshare first, and on to submitblock when the node says it
// is a block too. A share the node refuses (it went stale, say) costs nothing but itself.
func (f *NodeFeed) Submit(j *work.Job, nonce uint64) (bool, bool, error) {
	f.init()
	f.mu.Lock()
	took, settled := f.done[j]
	f.mu.Unlock()
	if settled {
		return took, false, nil
	}
	if j.Shares {
		block, err := f.Src.SubmitShare(j.ShareAt(nonce))
		switch {
		case errors.Is(err, nodeclient.ErrRPC):
			f.Log.Printf("lane %s: share refused: %v", j.Lane, err)
			return false, false, nil
		case err != nil:
			return false, false, err
		case !block:
			return true, false, nil
		}
	}
	return f.submitBlock(j, nonce)
}

func (f *NodeFeed) submitBlock(j *work.Job, nonce uint64) (bool, bool, error) {
	b := j.Block(nonce)
	id := b.Header.ID()

	start := time.Now()
	err := f.Src.SubmitBlock(b)
	unanswered := false
	for try := 0; err != nil && !errors.Is(err, nodeclient.ErrRPC); try++ {
		unanswered = true
		if try == submitRetries || time.Since(start) > submitWindow {
			break
		}
		f.Log.Printf("lane %s: block %d did not reach the node: %v — sending it again", j.Lane, b.Header.Height, err)
		time.Sleep(f.Retry << try)
		err = f.Src.SubmitBlock(b)
	}
	// A request that got no answer may still have arrived. The node then refuses the second copy as
	// one it already has, or never answers at all — and in both cases its tip is this block.
	if err != nil && unanswered {
		if _, tip, terr := f.Src.Tip(); terr == nil && tip == id {
			err = nil
		}
	}
	f.kickNow()
	switch {
	case err == nil:
		f.settle(j, true)
		f.Log.Printf("BLOCK FOUND and accepted: lane %s, height %d, %x", j.Lane, b.Header.Height, id[:8])
		return true, true, nil
	case errors.Is(err, nodeclient.ErrRPC):
		f.settle(j, false)
		f.Log.Printf("lane %s: block %d rejected: %v", j.Lane, b.Header.Height, err)
		return false, false, nil
	default:
		f.Log.Printf("lane %s: block %d was not delivered: %v", j.Lane, b.Header.Height, err)
		return false, false, err
	}
}

func (f *NodeFeed) settle(j *work.Job, took bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.done[j] = took
	if len(f.done) > 64 {
		f.done = map[*work.Job]bool{j: took}
	}
}

func (f *NodeFeed) kickNow() {
	select {
	case f.kick <- struct{}{}:
	default:
	}
}
