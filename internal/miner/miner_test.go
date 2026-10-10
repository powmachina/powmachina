package miner

import (
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"powmachina/internal/chain"
	"powmachina/internal/mh"
	"powmachina/internal/nodeclient"
	"powmachina/internal/work"
)

type fakeNode struct {
	mu     sync.Mutex
	tip    [32]byte
	height uint64
	got    []*chain.Block
}

func (f *fakeNode) Template(lane string) (*nodeclient.Template, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if lane != "C" {
		return nil, errors.New("lane C only")
	}
	return &nodeclient.Template{Height: f.height + 1, Lane: "C",
		Bits: fmt.Sprintf("%08x", chain.RegtestTargetBits), PrevBlock: hex.EncodeToString(f.tip[:]),
		MinTime: 1_700_000_001}, nil
}

func (f *fakeNode) SubmitBlock(b *chain.Block) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if b.Header.PrevBlock != f.tip {
		return errors.New("stale")
	}
	f.got = append(f.got, b)
	f.tip, f.height = b.Header.ID(), uint64(b.Header.Height)
	return nil
}

func (f *fakeNode) Tip() (uint64, [32]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.height, f.tip, nil
}

// The miner mines blocks and the node verifies them exactly as powmachinad does (Header.CheckPoW with
// the node's verifier) — no other PoW implementation is in play.
func TestMinerFindsValidBlocks(t *testing.T) {
	payout := make([]byte, 32)
	payout[0] = 7
	node := &fakeNode{tip: chain.RegtestGenesis().ID()}
	lg := log.New(io.Discard, "", 0)
	m := New(Options{Params: chain.RegTest, Threads: 4, Light: true, Log: lg})
	feed := &NodeFeed{Src: node, Payout: payout, Params: chain.RegTest, Poll: 20 * time.Millisecond, Log: lg}
	quit := make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- m.Run(feed, quit) }()
	deadline := time.Now().Add(2 * time.Minute)
	for {
		node.mu.Lock()
		n := len(node.got)
		node.mu.Unlock()
		if n >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d blocks found in 2 minutes", n)
		}
		time.Sleep(50 * time.Millisecond)
	}
	close(quit)
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	v := mh.NewVerifier(0, mh.TestProfile)
	rx, err := mh.NewRandomX(0, false)
	if err != nil {
		t.Fatal(err)
	}
	defer rx.Close()
	h, err := mh.NewHasherWithRandomX(rx)
	if err != nil {
		t.Fatal(err)
	}
	for i, b := range node.got[:2] {
		ok, err := b.Header.CheckPoW(h, v)
		if err != nil || !ok {
			t.Fatalf("the block %d: PoW failed the node's verification (%v)", i, err)
		}
		if b.Header.Lane != mh.LaneC || uint64(b.Header.Height) != uint64(i+1) {
			t.Fatalf("the block %d: lane %v height %d", i, b.Header.Lane, b.Header.Height)
		}
		if chain.TxMerkleRoot(b.Txs) != b.Header.MerkleRoot {
			t.Fatalf("the block %d: the merkle root does not match", i)
		}
		cb := b.Txs[0]
		paid := false
		for _, o := range cb.Outputs {
			if string(o.Script) == string(payout) && o.Value == chain.MinerSubsidy(uint64(b.Header.Height)) {
				paid = true
			}
		}
		if !paid {
			t.Fatalf("the block %d: the coinbase does not pay the miner its reward", i)
		}
	}
	if m.Blocks.Load() < 2 || m.Rejected.Load() != 0 {
		t.Fatalf("%d accepted, %d rejected", m.Accepted.Load(), m.Rejected.Load())
	}
}

// Two jobs from the same template have a different coinbase (the extranonce) — two miners with one
// address do not hash the same thing.
func TestJobsDiffer(t *testing.T) {
	node := &fakeNode{tip: chain.RegtestGenesis().ID()}
	tpl, _ := node.Template("C")
	a, err := work.Build(tpl, make([]byte, 32), chain.RegTest)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := work.Build(tpl, make([]byte, 32), chain.RegTest)
	if a.Header.MerkleRoot == b.Header.MerkleRoot {
		t.Fatal("two jobs have the same merkle root")
	}
	if a.Header.Time < tpl.MinTime {
		t.Fatal("the header's time is below min_time")
	}
}

// fakeGPU computes lane G through internal/mh (light) — a test of the GPU branch's logic without a card.
type fakeGPU struct {
	v      *mh.Verifier
	h      *mh.Hasher
	hdr    [80]byte
	target [32]byte
	broken bool
}

func (g *fakeGPU) SetEpoch(cache []uint32, items uint32, ops [16][3]uint8) error {
	g.v = mh.NewLightVerifier(0, mh.TestProfile)
	g.h = mh.NewHasher()
	return nil
}
func (g *fakeGPU) SetJob(h *[80]byte, t *[32]byte) error { g.hdr, g.target = *h, *t; return nil }
func (g *fakeGPU) Search(start uint64) ([]uint64, uint64, error) {
	var hits []uint64
	for n := start; n < start+64; n++ {
		out, _ := g.h.Hash(g.v, &g.hdr, n, mh.LaneG)
		if mh.MeetsTarget(&out, &g.target) {
			hits = append(hits, n)
		}
	}
	return hits, 64, nil
}
func (g *fakeGPU) Hash(h *[80]byte, n uint64) ([32]byte, error) {
	out, err := g.h.Hash(g.v, h, n, mh.LaneG)
	if g.broken {
		out[31] ^= 1 // one bit — exactly what would split the network
	}
	return out, err
}
func (g *fakeGPU) Config() (int, int, uint32) { return 1, 1, 64 }

type laneGNode struct{ fakeNode }

func (f *laneGNode) Template(lane string) (*nodeclient.Template, error) {
	t, _ := f.fakeNode.Template("C")
	t.Lane = lane
	return t, nil
}

func TestGPULaneMinesAndBrokenCardIsDisabled(t *testing.T) {
	if raceEnabled {
		// The fake card hashes with the reference code and under -race it is orders of magnitude slower:
		// without the instrumentation it finds two blocks in seconds, with it not even in four minutes. The
		// concurrency of this path is covered by the other tests, here only the instrumentation's speed would be measured.
		t.Skip("with -race the fake card is far too slow")
	}
	node := &laneGNode{fakeNode{tip: chain.RegtestGenesis().ID()}}
	good, bad := &fakeGPU{}, &fakeGPU{broken: true}
	var buf syncBuf
	lg := log.New(&buf, "", 0)
	m := New(Options{Params: chain.RegTest, Threads: -1, GPUs: []GPU{good, bad}, Log: lg})
	feed := &NodeFeed{Src: node, Payout: make([]byte, 32), Params: chain.RegTest, Poll: 20 * time.Millisecond, Log: lg}
	quit := make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- m.Run(feed, quit) }()
	deadline := time.Now().Add(time.Minute)
	for {
		node.mu.Lock()
		n := len(node.got)
		node.mu.Unlock()
		if n >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("lane G did not find the blocks before the deadline (%d)\n%s", n, buf.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	close(quit)
	<-done
	for _, b := range node.got {
		if b.Header.Lane != mh.LaneG {
			t.Fatal("the block is not from lane G")
		}
	}
	if !strings.Contains(buf.String(), "GPU 1: GPU hash differs from the reference") {
		t.Fatalf("the broken card was not turned off:\n%s", buf.String())
	}
	if m.DeviceHashes[2].Load() != 0 {
		t.Fatal("the broken card was hashing")
	}
}

type syncBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }
