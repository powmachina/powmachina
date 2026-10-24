package stratum

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"strings"
	"testing"

	"powmachina/internal/chain"
	"powmachina/internal/mh"
	"powmachina/internal/work"
)

// cheapNode is fakeNode with a PoW that costs nothing: the nonces in `good` are blocks and every
// other one is a weak share. The bookkeeping of shares is tested here, not the hash.
type cheapNode struct {
	*fakeNode
	good map[uint64]bool
}

func (n *cheapNode) PoWHash(h *chain.Header) ([32]byte, error) {
	var out [32]byte
	if !n.good[h.Nonce] {
		for i := range out {
			out[i] = 0xff
		}
	}
	return out, nil
}

// loggedIn returns a connection of the server that has logged in for lane G and has its first job.
// The other end of the connection is read and thrown away.
func loggedIn(t *testing.T, b Backend) (*sconn, string) {
	t.Helper()
	s := NewServer(b, ServerOptions{Log: log.New(io.Discard, "", 0)})
	near, far := net.Pipe()
	t.Cleanup(func() { near.Close(); far.Close() })
	go io.Copy(io.Discard, far)

	c := newSconn(s, near, "127.0.0.1")
	raw, _ := json.Marshal(LoginParams{Login: testAddr(9) + ".rig1", Lanes: []string{"G"}})
	if e := c.login(raw); e != nil {
		t.Fatalf("login: %v", e)
	}
	c.pushJobs()
	c.mu.Lock()
	defer c.mu.Unlock()
	jobs := c.lanes[mh.LaneG].jobs
	if len(jobs) != 1 {
		t.Fatalf("the connection has %d jobs after the login", len(jobs))
	}
	return c, jobs[0].ID
}

// submitNow sends a share past the pace limit — the limit is not what is tested here.
func submitNow(c *sconn, jobID, nonce string) (*SubmitResult, *Error) {
	c.mu.Lock()
	c.tokens = 40
	c.mu.Unlock()
	raw, _ := json.Marshal(SubmitParams{JobID: jobID, Nonce: nonce})
	return c.submit(raw)
}

// A share is counted once for as long as its job lives, however many shares come after it.
//
// The shares already seen used to be one set per connection, emptied when it grew large. A miner
// that sent enough nonces to fill it could then send its accepted shares again and have them
// counted a second time.
func TestShareIsDuplicateForTheWholeLifeOfItsJob(t *testing.T) {
	const first = uint64(7)
	node := &cheapNode{fakeNode: &fakeNode{tip: chain.RegtestGenesis().ID()}, good: map[uint64]bool{first: true}}
	c, jobID := loggedIn(t, node)

	if res, e := submitNow(c, jobID, FormatNonce(first)); e != nil || res.Status != "OK" {
		t.Fatalf("the first share: %v", e)
	}
	// More nonces than any bound a set of seen shares ever had, and new jobs in between.
	for n := uint64(1000); n < 1000+120_000; n++ {
		if _, e := submitNow(c, jobID, FormatNonce(n)); e == nil || e.Code != ErrCodeLowDiff {
			t.Fatalf("nonce %d: %v", n, e)
		}
		if n%40_000 == 0 {
			c.pushJob(mh.LaneG)
		}
	}
	c.pushJob(mh.LaneG)

	if _, e := submitNow(c, jobID, FormatNonce(first)); e == nil || e.Code != ErrCodeDuplicate {
		t.Fatalf("a share sent again after 120 000 others was not a duplicate: %v", e)
	}
	c.mu.Lock()
	accepted := c.accepted
	c.mu.Unlock()
	if accepted != 1 {
		t.Fatalf("%d shares were accepted, one was found", accepted)
	}
}

// The same nonce written in another way is the same share. Hex digits may be in either case, so a
// nonce with a letter in it has more than one spelling.
func TestShareIsDuplicateHoweverItsNonceIsWritten(t *testing.T) {
	const nonce = uint64(0x00ab_cdef_0000_00fa)
	node := &cheapNode{fakeNode: &fakeNode{tip: chain.RegtestGenesis().ID()}, good: map[uint64]bool{nonce: true}}
	c, jobID := loggedIn(t, node)

	lower := FormatNonce(nonce)
	if res, e := submitNow(c, jobID, lower); e != nil || res.Status != "OK" {
		t.Fatalf("the first share: %v", e)
	}
	for _, again := range []string{lower, strings.ToUpper(lower), fmt.Sprintf("%016X", nonce)[:8] + lower[8:]} {
		if _, e := submitNow(c, jobID, again); e == nil || e.Code != ErrCodeDuplicate {
			t.Errorf("nonce %s sent again as %s was not a duplicate: %v", lower, again, e)
		}
	}
}

// A job that has been dropped takes its nonces with it, and a share for it is stale — not new.
func TestShareForADroppedJobIsStale(t *testing.T) {
	const nonce = uint64(7)
	node := &cheapNode{fakeNode: &fakeNode{tip: chain.RegtestGenesis().ID()}, good: map[uint64]bool{nonce: true}}
	c, jobID := loggedIn(t, node)

	if _, e := submitNow(c, jobID, FormatNonce(nonce)); e != nil {
		t.Fatalf("the first share: %v", e)
	}
	// The connection keeps the last eight jobs of a lane.
	for i := 0; i < 8; i++ {
		c.pushJob(mh.LaneG)
	}
	if _, e := submitNow(c, jobID, FormatNonce(nonce)); e == nil || e.Code != ErrCodeStale {
		t.Fatalf("a share for a dropped job: %v", e)
	}
}

// A lane the rotation rule blocks still gets work on a node that takes shares: every hit is a
// Sharepool share, also one at the block's target — nothing is submitted as a block.
func TestBlockedLaneMinesSharesOnly(t *testing.T) {
	for _, noBlock := range []bool{false, true} {
		node := &cheapNode{fakeNode: &fakeNode{tip: chain.RegtestGenesis().ID(), takesShares: true, noBlock: noBlock},
			good: map[uint64]bool{7: true}}
		c, job := loggedIn(t, node)
		res, e := submitNow(c, job, FormatNonce(7))
		if e != nil {
			t.Fatalf("noBlock=%v: %v", noBlock, e)
		}
		node.mu.Lock()
		blocks, shares := len(node.blocks), len(node.shares)
		node.mu.Unlock()
		if noBlock && (res.Block || blocks != 0 || shares != 1) {
			t.Fatalf("a blocked lane: block %v, %d blocks, %d shares; want a share only", res.Block, blocks, shares)
		}
		if !noBlock && (!res.Block || blocks != 1) {
			t.Fatalf("an allowed lane: block %v, %d blocks; want the block", res.Block, blocks)
		}
	}
}

// The easiest target a device gets is the lightest Sharepool share on a node that takes shares,
// and the network's floor on one that does not.
func TestEasiestTargetFollowsTheSharepool(t *testing.T) {
	bits := chain.RegtestTargetBits
	st0, _ := chain.ShareTarget(0x1e00ffff, 0)
	got := easiestTarget(&work.Template{TakesShares: true}, 0x1e00ffff, chain.TestNet)
	if got.Cmp(new(big.Int).SetBytes(st0[:])) != 0 {
		t.Fatalf("a node that takes shares: %x, want the weight-0 share target %x", got, st0)
	}
	limit, _ := chain.CompactToTarget(chain.TestNet.PowLimitBits)
	if got := easiestTarget(&work.Template{}, bits, chain.TestNet); got.Cmp(new(big.Int).SetBytes(limit[:])) != 0 {
		t.Fatalf("a node that takes no shares: %x, want the network's floor", got)
	}
}
