package miner

import (
	"errors"
	"log"
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
	Tip() (uint64, [32]byte, error)
}

// NodeFeed is solo mining against your own node: the template through gettemplate, the coinbase to your
// own address (work.Build), the block through submitblock.
type NodeFeed struct {
	Src     Source
	Payout  []byte
	Params  chain.NetParams
	Refresh time.Duration // a new template even without a new block (new transactions, the time); 0 = 20 s
	Poll    time.Duration // how often to ask for a new tip; 0 = 1 s
	Log     *log.Logger

	mu     sync.Mutex
	done   map[*work.Job]bool
	kick   chan struct{}
	logged map[mh.Lane]bool
}

func (f *NodeFeed) init() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.done == nil {
		f.done = map[*work.Job]bool{}
		f.kick = make(chan struct{}, 1)
		f.logged = map[mh.Lane]bool{}
	}
	if f.Refresh <= 0 {
		f.Refresh = 20 * time.Second
	}
	if f.Poll <= 0 {
		f.Poll = time.Second
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

func (f *NodeFeed) job(l mh.Lane) (*work.Job, error) {
	t, err := f.Src.Template(l.String())
	if err != nil {
		return nil, err
	}
	return work.Build(t, f.Payout, f.Params)
}

// Submit implements Feed: it sends the block of a job to the node. A job a block has already gone out
// of does not send further nonces (the node would refuse them as duplicates).
func (f *NodeFeed) Submit(j *work.Job, nonce uint64) (bool, bool, error) {
	f.init()
	f.mu.Lock()
	if f.done[j] {
		f.mu.Unlock()
		return true, false, nil
	}
	f.done[j] = true
	if len(f.done) > 64 {
		f.done = map[*work.Job]bool{j: true}
	}
	f.mu.Unlock()
	b := j.Block(nonce)
	id := b.Header.ID()
	if err := f.Src.SubmitBlock(b); err != nil {
		f.Log.Printf("lane %s: block %d rejected: %v", j.Lane, b.Header.Height, err)
		f.kickNow()
		if errors.Is(err, nodeclient.ErrRPC) {
			return false, false, nil
		}
		return false, false, err
	}
	f.Log.Printf("BLOCK FOUND and accepted: lane %s, height %d, %x", j.Lane, b.Header.Height, id[:8])
	f.kickNow()
	return true, true, nil
}

func (f *NodeFeed) kickNow() {
	select {
	case f.kick <- struct{}{}:
	default:
	}
}
