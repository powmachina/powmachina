package miner

import (
	"errors"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"powmachina/internal/chain"
	"powmachina/internal/mh"
	"powmachina/internal/work"
)

// pickyGPU is a card that cannot be set up for some epochs, or not at the first attempt. Like
// fakeGPU it hashes with the reference code.
type pickyGPU struct {
	mu       sync.Mutex
	epochs   map[[16][3]uint8]uint64 // the epoch a RANDMATH sequence belongs to
	tooLarge map[uint64]bool         // the epochs whose dataset does not fit the card
	failNext int                     // this many of the next attempts fail whatever the epoch
	attempts int

	v      *mh.Verifier
	h      *mh.Hasher
	hdr    [80]byte
	target [32]byte
}

func newPickyGPU(tooLarge ...uint64) *pickyGPU {
	g := &pickyGPU{epochs: map[[16][3]uint8]uint64{}, tooLarge: map[uint64]bool{}, h: mh.NewHasher()}
	for e := uint64(0); e < 4; e++ {
		g.epochs[mh.NewRandMath(e).OpsRaw()] = e
	}
	for _, e := range tooLarge {
		g.tooLarge[e] = true
	}
	return g
}

func (g *pickyGPU) SetEpoch(cache []uint32, items uint32, ops [16][3]uint8) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.attempts++
	epoch, ok := g.epochs[ops]
	if !ok {
		return errors.New("an epoch the test did not expect")
	}
	if g.failNext > 0 {
		g.failNext--
		return errors.New("out of memory")
	}
	if g.tooLarge[epoch] {
		return errors.New("the dataset does not fit the card")
	}
	g.v = mh.NewLightVerifier(epoch, mh.TestProfile)
	return nil
}

func (g *pickyGPU) Attempts() int { g.mu.Lock(); defer g.mu.Unlock(); return g.attempts }

func (g *pickyGPU) SetJob(h *[80]byte, t *[32]byte) error { g.hdr, g.target = *h, *t; return nil }
func (g *pickyGPU) Search(start uint64) ([]uint64, uint64, error) {
	for n := start; n < start+4; n++ {
		if _, err := g.h.Hash(g.v, &g.hdr, n, mh.LaneG); err != nil {
			return nil, 0, err
		}
	}
	return nil, 4, nil
}
func (g *pickyGPU) Hash(h *[80]byte, n uint64) ([32]byte, error) {
	return g.h.Hash(g.v, h, n, mh.LaneG)
}
func (g *pickyGPU) Config() (int, int, uint32) { return 1, 1, 4 }

// laneGMiner is a miner with one card and no feed: the test hands it the jobs itself.
func laneGMiner(t *testing.T, g GPU, retry time.Duration) (*Miner, *syncBuf) {
	t.Helper()
	old := gpuEpochRetry
	gpuEpochRetry = retry
	buf := &syncBuf{}
	m := New(Options{Params: chain.RegTest, Threads: -1, GPUs: []GPU{g}, Log: log.New(buf, "", 0)})
	m.startGPUs()
	t.Cleanup(func() {
		m.close()
		gpuEpochRetry = old
	})
	return m, buf
}

func waitFor(t *testing.T, what string, buf *syncBuf, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(time.Minute); !cond(); {
		if time.Now().After(deadline) {
			t.Fatalf("%s\n%s", what, buf.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A job names its epoch, and a pool can name one whose dataset is larger than the card's memory.
// The card cannot mine that epoch — but the next job of an epoch it can hold has to bring it back.
// It used to be switched off until the miner was restarted, while the CPU mined on and the owner
// had no reason to look.
func TestCardThatCannotHoldOneEpochMinesTheNext(t *testing.T) {
	g := newPickyGPU(1)
	m, buf := laneGMiner(t, g, time.Hour)

	m.setJob(mh.LaneG, &work.Job{Lane: mh.LaneG, Epoch: 1})
	waitFor(t, "the card never tried the epoch", buf, func() bool {
		return strings.Contains(buf.String(), "GPU 0: the dataset does not fit the card")
	})
	// The same epoch is not tried over and over while nothing has changed.
	time.Sleep(300 * time.Millisecond)
	if n := g.Attempts(); n != 1 {
		t.Fatalf("the card tried the epoch it cannot hold %d times", n)
	}
	if n := m.DeviceHashes[1].Load(); n != 0 {
		t.Fatalf("the card hashed %d times for an epoch it could not be set up for", n)
	}

	m.setJob(mh.LaneG, &work.Job{Lane: mh.LaneG, Epoch: 0})
	waitFor(t, "the card did not come back for an epoch it can hold", buf, func() bool {
		return m.DeviceHashes[1].Load() > 0
	})
}

// A card that could not be set up for a reason that passes — memory another program held for a
// while — tries the same epoch again by itself. An epoch lasts a week; waiting for the next one is
// a week without the card.
func TestCardTriesTheSameEpochAgainAfterAWhile(t *testing.T) {
	g := newPickyGPU()
	g.failNext = 2
	m, buf := laneGMiner(t, g, 100*time.Millisecond)

	m.setJob(mh.LaneG, &work.Job{Lane: mh.LaneG, Epoch: 0})
	waitFor(t, "the card did not come back after the failures passed", buf, func() bool {
		return m.DeviceHashes[1].Load() > 0
	})
	if n := g.Attempts(); n != 3 {
		t.Fatalf("the card was set up after %d attempts, two were to fail", n)
	}
}
