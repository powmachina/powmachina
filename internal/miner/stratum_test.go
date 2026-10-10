package miner

import (
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"testing"
	"time"

	"powmachina/internal/address"
	"powmachina/internal/chain"
	"powmachina/internal/mh"
	"powmachina/internal/stratum"
	"powmachina/internal/work"
)

// stratumNode is the stratum server's backend: regtest in memory, PoW with the same code as the node.
type stratumNode struct {
	mu     sync.Mutex
	tip    [32]byte
	height uint64
	blocks []*chain.Block
	v      *mh.Verifier
	h      *mh.Hasher
}

func (f *stratumNode) Params() chain.NetParams { return chain.RegTest }
func (f *stratumNode) TipID() [32]byte         { f.mu.Lock(); defer f.mu.Unlock(); return f.tip }
func (f *stratumNode) WorkTemplate(lane mh.Lane) (*work.Template, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return &work.Template{Height: f.height + 1, Lane: lane.String(), Bits: fmt.Sprintf("%08x", chain.RegtestTargetBits),
		PrevBlock: hex.EncodeToString(f.tip[:]), MinTime: 1_700_000_001}, nil
}
func (f *stratumNode) PoWHash(h *chain.Header) ([32]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	pow := h.PoWInput()
	return f.h.Hash(f.v, &pow, h.Nonce, h.Lane)
}
func (f *stratumNode) SubmitBlock(b *chain.Block) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if b.Header.PrevBlock != f.tip {
		return fmt.Errorf("stale")
	}
	f.blocks = append(f.blocks, b)
	f.tip, f.height = b.Header.ID(), uint64(b.Header.Height)
	return nil
}

// The miner over stratum: the CPU mines lane C on the node's stratum server, the blocks pay the address from the login.
func TestMinerOverStratum(t *testing.T) {
	rx, err := mh.NewRandomX(0, false)
	if err != nil {
		t.Skipf("RandomX: %v", err)
	}
	h, _ := mh.NewHasherWithRandomX(rx)
	node := &stratumNode{tip: chain.RegtestGenesis().ID(), v: mh.NewLightVerifier(0, mh.TestProfile), h: h}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	quietLog := log.New(io.Discard, "", 0)
	srv := stratum.NewServer(node, stratum.ServerOptions{Log: quietLog})
	go srv.Serve(ln)
	defer srv.Close()

	k := make([]byte, 32)
	k[0] = 5
	addr, _ := address.Encode(chain.RegTest.Bech32HRP, chain.ScriptVersionBasic, k)
	m := New(Options{Params: chain.RegTest, Threads: 2, Light: true, Log: quietLog})
	feed := &stratum.Client{URL: "stratum+tcp://" + ln.Addr().String(), Login: addr + ".test", Agent: "test", Log: quietLog}
	quit := make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- m.Run(feed, quit) }()
	deadline := time.Now().Add(2 * time.Minute)
	for {
		node.mu.Lock()
		n := len(node.blocks)
		node.mu.Unlock()
		if n >= 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d blocks over stratum in 2 minutes", n)
		}
		time.Sleep(50 * time.Millisecond)
	}
	st := srv.Stats()
	if len(st) != 1 || st[0].Worker != "test" || st[0].Blocks < 2 {
		t.Fatalf("the server's statistics: %+v", st)
	}
	// The miner only counts a block once the server confirms the submit, so the node has it slightly
	// earlier than the miner's counter rises. We wait, otherwise the test fails depending on how fast the
	// machine is (on the CI runner it reported 2 of 3).
	for deadline := time.Now().Add(15 * time.Second); m.Blocks.Load() < 3; {
		if time.Now().After(deadline) {
			t.Fatalf("the miner reports %d blocks", m.Blocks.Load())
		}
		time.Sleep(20 * time.Millisecond)
	}
	close(quit)
	<-done
	for i, b := range node.blocks[:3] {
		if string(b.Txs[0].Outputs[len(b.Txs[0].Outputs)-1].Script) != string(k) {
			t.Fatalf("the block %d does not pay the address from the login", i)
		}
	}
}
