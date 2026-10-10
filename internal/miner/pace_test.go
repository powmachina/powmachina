package miner

import (
	"encoding/binary"
	"io"
	"log"
	"math/big"
	"sync"
	"testing"
	"time"

	"powmachina/internal/chain"
	"powmachina/internal/mh"
	"powmachina/internal/work"
)

func targetOf(n *big.Int) (out [32]byte) {
	n.FillBytes(out[:])
	return out
}

// A tightened target is the job's target halved, and it stops at the block's: past that the miner would
// be throwing blocks away.
func TestTaskTargetStopsAtTheBlock(t *testing.T) {
	block, err := chain.CompactToTarget(chain.RegtestTargetBits)
	if err != nil {
		t.Fatal(err)
	}
	blockN := new(big.Int).SetBytes(block[:])
	j := &work.Job{Lane: mh.LaneC, Target: targetOf(new(big.Int).Lsh(blockN, 4))} // a share 16× easier than a block
	binary.LittleEndian.PutUint32(j.PoW[72:76], chain.RegtestTargetBits)
	tk := newTask(j)
	if tk.block != block {
		t.Fatalf("the block target read from the header is %x, want %x", tk.block, block)
	}
	if got := tk.target(0); got != j.Target {
		t.Fatal("an untightened target is not the job's")
	}
	if got, want := tk.target(3), targetOf(new(big.Int).Lsh(blockN, 1)); got != want {
		t.Fatalf("three halvings give %x, want %x", got, want)
	}
	for _, n := range []uint32{4, 5, 64} {
		if got := tk.target(n); got != block {
			t.Fatalf("%d halvings went past the block target: %x", n, got)
		}
	}
	// The node's own stratum at the easiest difficulty: the job's target is the block's already.
	j.Target = block
	if got := newTask(j).target(7); got != block {
		t.Fatalf("a job at the block target was tightened: %x", got)
	}
}

func TestRetune(t *testing.T) {
	l := &lane{id: mh.LaneC}
	t0 := time.Unix(1_000, 0)
	l.retune(t0) // the first call only starts the clock

	l.seen = 94 // 47 a second: what a 740 H/s CPU finds at a share target of 16 hashes
	n, changed := l.retune(t0.Add(2 * time.Second))
	if !changed || n != 6 {
		t.Fatalf("47/s gave %d halvings (changed %v), want 6", n, changed)
	}

	l.seen = 2 // the aim: under a solution a second, nothing to do
	if n, changed = l.retune(t0.Add(4 * time.Second)); changed {
		t.Fatalf("1/s changed the target to %d", n)
	}
	l.seen = 6
	if n, changed = l.retune(t0.Add(12 * time.Second)); changed {
		t.Fatalf("0.8/s over ten seconds changed the target to %d", n)
	}

	// The pool raised its difficulty: the lane is quiet, so it gives the halvings back one at a time.
	l.seen = 1
	if n, changed = l.retune(t0.Add(22 * time.Second)); !changed || n != 5 {
		t.Fatalf("a quiet lane went to %d (changed %v), want 5", n, changed)
	}
	l.tighten.Store(0)
	l.seen = 0
	if n, changed = l.retune(t0.Add(40 * time.Second)); changed || n != 0 {
		t.Fatalf("a quiet lane with nothing to give back changed to %d", n)
	}
}

// shareFeed hands out one lane C job whose share target every hash meets, and records what comes back.
type shareFeed struct {
	job *work.Job
	mu  sync.Mutex
	got []uint64
}

func (f *shareFeed) Run(lanes []mh.Lane, set func(mh.Lane, *work.Job), quit <-chan struct{}) error {
	set(mh.LaneC, f.job)
	<-quit
	return nil
}

func (f *shareFeed) Submit(j *work.Job, nonce uint64) (bool, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = append(f.got, nonce)
	return true, false, nil
}

// A share target so easy that every hash is a share, far more than the lane may send. Every nonce
// whose hash is a block has to reach the pool all the same.
func TestBlocksSurviveAFloodOfShares(t *testing.T) {
	if raceEnabled {
		t.Skip("with -race light RandomX is far too slow")
	}
	j := &work.Job{ID: "C1", Lane: mh.LaneC, Height: 1}
	for i := range j.Target {
		j.Target[i] = 0xff
	}
	j.PoW[0] = 1
	const bits = 0x2003ffff // one hash in 64 is a block
	binary.LittleEndian.PutUint32(j.PoW[72:76], bits)
	block, err := chain.CompactToTarget(bits)
	if err != nil {
		t.Fatal(err)
	}

	feed := &shareFeed{job: j}
	m := New(Options{Params: chain.RegTest, Threads: 2, Light: true, NoAffinity: true, Log: log.New(io.Discard, "", 0)})
	quit := make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- m.Run(feed, quit) }()

	// Long enough for the lane to notice the flood and tighten, and for dozens of blocks to turn up.
	deadline := time.Now().Add(2 * time.Minute)
	for m.lanes[mh.LaneC].tighten.Load() == 0 || m.Hashes() < 2048 {
		if time.Now().After(deadline) {
			t.Fatalf("the lane never tightened (%d hashes)", m.Hashes())
		}
		time.Sleep(50 * time.Millisecond)
	}
	close(quit)
	<-done

	// What the miner covered: the two threads walk nonces 0, 2, 4… and 1, 3, 5…, both to the same depth
	// give or take a couple of batches. Check the range every thread certainly finished.
	covered := m.Hashes() - 1024
	rx, err := mh.NewRandomXForMiner(j.Epoch, false, false, false)
	if err != nil {
		t.Fatal(err)
	}
	defer rx.Close()
	h, err := mh.NewHasherWithRandomX(rx)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	sent := map[uint64]bool{}
	feed.mu.Lock()
	for _, n := range feed.got {
		sent[n] = true
	}
	total := len(feed.got)
	feed.mu.Unlock()
	blocks := 0
	for n := uint64(0); n < covered; n++ {
		hdr := j.PoW
		out, err := h.HashLaneC(&hdr, n)
		if err != nil {
			t.Fatal(err)
		}
		if !mh.MeetsTarget(&out, &block) {
			continue
		}
		blocks++
		if !sent[n] {
			t.Errorf("nonce %d is a block and was never submitted", n)
		}
	}
	if blocks == 0 {
		t.Fatalf("no block among %d nonces — the test proved nothing", covered)
	}
	t.Logf("%d hashes, %d submitted, %d of them had to be and were", m.Hashes(), total, blocks)
}

// A lane that had work for 10 of 40 seconds and did 5,000 hashes in them hashes at 500 a second, not
// at 125.
func TestMeasureLeavesOutSecondsWithoutWork(t *testing.T) {
	l := &lane{id: mh.LaneG}
	job := &task{job: &work.Job{Lane: mh.LaneG}}
	t0 := time.Unix(2_000, 0)
	var hashes uint64
	var got float64
	var reported bool
	for s := 0; s <= 40; s++ {
		busy := s >= 5 && s <= 15 // work from second 5 to second 15
		if busy {
			l.job.Store(job)
		} else {
			l.job.Store(nil)
		}
		if s > 5 && s <= 15 {
			hashes += 500
		}
		if rate, ok := l.measure(t0.Add(time.Duration(s)*time.Second), hashes); ok {
			got, reported = rate, true
		}
	}
	if !reported {
		t.Fatal("nothing was reported in 40 seconds")
	}
	if got != 500 {
		t.Fatalf("the rate is %.1f H/s, want 500", got)
	}

	// Under five seconds of work in a stretch says nothing — it is carried over, not reported.
	l = &lane{id: mh.LaneG}
	l.job.Store(job)
	l.measure(t0, 0)
	l.measure(t0.Add(2*time.Second), 1000)
	l.job.Store(nil)
	if _, ok := l.measure(t0.Add(40*time.Second), 1000); ok {
		t.Fatal("two seconds of work were reported as a rate")
	}
}
