package stratum

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"io"
	"log"
	"strings"
	"testing"
	"time"

	"powmachina/internal/chain"
	"powmachina/internal/finkey"
	"powmachina/internal/mh"
	"powmachina/internal/work"
)

// A miner that logs in with its own finality key mines blocks that carry that key, and it signs
// the blocks its key has a seat for itself: the vote of its work is its own, not the server's.
// A miner without a key mines with the node's.
func TestMinerWithItsOwnFinalityKey(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer := finkey.New(priv, "")
	nodeKey := strings.Repeat("11", 32)
	block := [32]byte{0xab}
	f := &fakeNode{tip: chain.RegtestGenesis().ID(), nodeKey: nodeKey,
		due: map[[32]byte]dueAttest{signer.Pub: {height: 5, block: block}}}
	s, addr := startServer(t, f, ServerOptions{})

	run := func(fin *finkey.Signer) (stop func()) {
		cl := &Client{URL: addr, Login: testAddr(3) + ".rig", Log: log.New(io.Discard, "", 0), Finality: fin}
		quit := make(chan struct{})
		done := make(chan struct{})
		go func() { cl.Run([]mh.Lane{mh.LaneG}, func(mh.Lane, *work.Job) {}, quit); close(done) }()
		return func() { close(quit); <-done }
	}
	stop := run(signer)
	defer stop()

	// The signature arrives without the miner doing anything but mining.
	deadline := time.Now().Add(20 * time.Second)
	for {
		f.mu.Lock()
		n := len(f.atts)
		f.mu.Unlock()
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the miner did not sign the block its key has a seat for")
		}
		time.Sleep(10 * time.Millisecond)
	}
	f.mu.Lock()
	a := f.atts[0]
	f.mu.Unlock()
	if a.Key != signer.Pub || a.Height != 5 || a.BlockID != block || a.Verify() != nil {
		t.Fatalf("the attestation: height %d key %x block %x verify %v", a.Height, a.Key[:4], a.BlockID[:4], a.Verify())
	}

	stopPlain := run(nil)
	defer stopPlain()
	keys := func() map[string]bool {
		out := map[string]bool{}
		for _, c := range s.all() {
			c.mu.Lock()
			if st := c.lanes[mh.LaneG]; st != nil && len(st.jobs) > 0 {
				k := st.jobs[len(st.jobs)-1].ShareAt(0).Commit.FinalityKey
				out[hex.EncodeToString(k[:])] = true
			}
			c.mu.Unlock()
		}
		return out
	}
	for deadline := time.Now().Add(20 * time.Second); len(keys()) < 2; {
		if time.Now().After(deadline) {
			t.Fatalf("the keys in the jobs: %v", keys())
		}
		time.Sleep(10 * time.Millisecond)
	}
	got := keys()
	if !got[hex.EncodeToString(signer.Pub[:])] || !got[nodeKey] {
		t.Fatalf("the jobs carry %v; want the miner's key and the node's", got)
	}
}

// A login with a finality key that is not 32 bytes is refused.
func TestLoginWithABadFinalityKey(t *testing.T) {
	f := &fakeNode{tip: chain.RegtestGenesis().ID()}
	_, addr := startServer(t, f, ServerOptions{})
	r := dialRaw(t, addr)
	r.send(1, "login", LoginParams{Login: testAddr(3), Lanes: []string{"C"}, FinalityKey: "abcd"})
	if m := r.resp(t); m["error"] == nil {
		t.Fatalf("the login was taken: %s", m["result"])
	}
}
