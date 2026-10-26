package main

import (
	"bytes"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"strconv"
	"sync"
	"testing"

	"powmachina/internal/chain"
	"powmachina/internal/mh"
	"powmachina/internal/p2p"
)

// Finality has to hold while the chain reorganises under it. Three miners with their own finality
// keys mine apart and catch up with each other, so they reorganise, and each signs at its own tip
// whatever branch it is on. One of them is a node on disk that is restarted now and then; it takes
// the others' branches while their attestations arrive at the same time, and it is offered branches
// that fork below what it has finalized, long enough to win on work. After every step:
//
//   - the finalized block is on the node's active branch, and never leaves it;
//   - finality only moves forward, a restart included;
//   - a branch is refused only when it does not hold the finalized block;
//   - no key ever signs two blocks at one height, across reorgs and restarts;
//   - with every attestation delivered, the node is final exactly where a node that synced its
//     active chain from scratch is.
//
// POWM_FINALITY_SEEDS runs more seeds than the default.
func TestFinalityHoldsThroughReorgsAndRestarts(t *testing.T) {
	seeds := 1
	if s, err := strconv.Atoi(os.Getenv("POWM_FINALITY_SEEDS")); err == nil && s > 0 {
		seeds = s
	}
	for seed := range seeds {
		t.Run(fmt.Sprintf("seed%d", seed), func(t *testing.T) { finalityScenario(t, uint64(seed)) })
	}
}

type finWorld struct {
	t        *testing.T
	rng      *rand.Rand
	branches []*Node
	atts     []chain.Attestation
	signed   map[[32]byte]map[uint64][32]byte // what each key signed, by height
}

// record files the attestations a node made with its own key; a second block at a height a key
// already signed fails the test.
func (w *finWorld) record(n *Node) {
	for _, a := range n.finality.InRange(0, math.MaxUint64, 1<<16) {
		if a.Key != n.signer.Pub {
			continue
		}
		m := w.signed[a.Key]
		if m == nil {
			m = map[uint64][32]byte{}
			w.signed[a.Key] = m
		}
		if prev, ok := m[a.Height]; ok {
			if prev != a.BlockID {
				w.t.Fatalf("key %x signed two blocks at height %d: %x and %x", a.Key[:4], a.Height, prev[:4], a.BlockID[:4])
			}
			continue
		}
		m[a.Height] = a.BlockID
		w.atts = append(w.atts, a)
	}
}

// mine adds blocks to a node's chain one by one, recording what it signs on the way.
func (w *finWorld) mine(n *Node, count int) {
	for range count {
		if err := n.MineBlocks(1); err != nil {
			w.t.Fatal(err)
		}
		w.record(n)
	}
}

// branch is a new miner with its own finality key that has `from` up to `height`.
func (w *finWorld) branch(from *Node, height uint64) *Node {
	n := NewNode(chain.RegTest, chain.RegtestGenesis(), mh.TestProfile)
	w.t.Cleanup(n.Close)
	n.minerKey = bytes.Repeat([]byte{0xD0 + byte(len(w.branches))}, 32)
	if from != nil && height > 0 {
		feedBranch(w.t, n, from, 1, height)
	}
	w.branches = append(w.branches, n)
	return n
}

// deliver hands every attestation there is to a node, in random order.
func (w *finWorld) deliver(n *Node) {
	atts := append([]chain.Attestation(nil), w.atts...)
	w.rng.Shuffle(len(atts), func(i, j int) { atts[i], atts[j] = atts[j], atts[i] })
	for _, a := range atts {
		_ = n.acceptAttestation(a, "")
	}
}

// offer gives `dst` what it does not have of `src`'s chain and returns the first refusal.
func offer(dst, src *Node) error {
	_, top, _ := src.chain.Tip()
	from := uint64(1)
	for ; from <= top; from++ {
		id, _ := src.chain.BlockIDAt(from)
		if !dst.chain.Has(id) || !dst.store.HasBody(id) {
			break
		}
	}
	if from > top {
		return nil
	}
	peer := p2p.NewPeer(p2p.Address{}, p2p.PeerInbound)
	var headers []*chain.Header
	var blocks []*chain.Block
	for h := from; h <= top; h++ {
		id, _ := src.chain.BlockIDAt(h)
		b, err := src.store.Block(id)
		if err != nil {
			return err
		}
		headers = append(headers, b.Header)
		blocks = append(blocks, b)
	}
	if _, err := dst.acceptHeaders(peer, headers); err != nil {
		return err
	}
	var first error
	for _, b := range blocks {
		if err := dst.acceptBlock(peer, b); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func finalityScenario(t *testing.T, seed uint64) {
	w := &finWorld{t: t, rng: rand.New(rand.NewPCG(seed, 0xF1A)), signed: map[[32]byte]map[uint64][32]byte{}}
	steps := 40
	if testing.Short() || raceEnabled {
		steps = 16
	}
	dir := t.TempDir()
	open := func() *Node {
		n, _, err := OpenNode(chain.RegTest, chain.RegtestGenesis(), mh.TestProfile, dir)
		if err != nil {
			t.Fatal(err)
		}
		n.minerKey = bytes.Repeat([]byte{0xCF}, 32)
		return n
	}
	subject := open()
	defer func() { subject.Close() }()

	// The three mine one chain together first, so all three hold seats: the node half of them.
	miners := []*Node{subject, w.branch(nil, 0), w.branch(nil, 0)}
	for i := range finalityBlocks + 5 {
		m := miners[[]int{0, 1, 0, 2}[i%4]]
		w.mine(m, 1)
		for _, o := range miners {
			if o != m {
				feedAll(t, o, m)
			}
		}
		for _, o := range miners {
			w.deliver(o)
		}
	}
	startH, _, _ := subject.finality.Finalized()

	var lastH uint64
	var lastID [32]byte
	var hadFinal bool
	refused, switches, deep, restarts := 0, 0, 0, 0
	check := func(step int, what string) {
		t.Helper()
		h, id, ok := subject.finality.Finalized()
		if hadFinal {
			if !ok || h < lastH {
				t.Fatalf("step %d (%s): finality went back from %d to %d (%v)", step, what, lastH, h, ok)
			}
			if have, _ := subject.chain.BlockIDAt(lastH); have != lastID {
				t.Fatalf("step %d (%s): finalized block %d left the active branch", step, what, lastH)
			}
		}
		if ok {
			if have, _ := subject.chain.BlockIDAt(h); have != id {
				t.Fatalf("step %d (%s): height %d is final with %x, the active branch has %x", step, what, h, id[:4], have[:4])
			}
			lastH, lastID, hadFinal = h, id, true
		}
		for _, m := range miners {
			w.record(m)
		}
	}
	// take offers `src` to the node while the attestations arrive from another goroutine, as from
	// peers. A heavier branch has to win when it holds the finalized block and lose when it does not.
	take := func(step int, what string, src *Node) {
		t.Helper()
		before := mustTipID(t, subject)
		h, id, final := subject.finality.Finalized()
		heavier := src.chain.Work().Cmp(subject.chain.Work()) > 0
		srcTip, _, srcID := src.chain.Tip()
		ourTip, _, _ := subject.chain.Tip()
		holds := true
		if final {
			have, _ := src.chain.BlockIDAt(h)
			holds = have == id
		}
		atts := append([]chain.Attestation(nil), w.atts...)
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			for _, a := range atts {
				_ = subject.acceptAttestation(a, "")
			}
		}()
		_ = offer(subject, src)
		wg.Wait()
		after := mustTipID(t, subject)
		switch {
		case !holds && after == srcID:
			t.Fatalf("step %d (%s): the node took a branch without its finalized block %d", step, what, h)
		case !holds && heavier:
			refused++
		case heavier && srcTip.PrevBlock != ourTip.PrevBlock && after != srcID:
			// A sibling of the tip is decided by votes, not work (siblings.go); anything else heavier wins.
			t.Fatalf("step %d (%s): a heavier branch holding the finalized block %d was not taken", step, what, h)
		}
		if after != before {
			switches++
		}
	}

	for step := range steps {
		what := ""
		switch r := w.rng.IntN(10); {
		case r < 3: // a miner mines on its own
			m := miners[w.rng.IntN(len(miners))]
			w.mine(m, 1+w.rng.IntN(5))
			what = "mine"
		case r < 5: // the node takes another miner's branch
			j := 1 + w.rng.IntN(len(miners)-1)
			w.mine(miners[j], w.rng.IntN(6))
			what = fmt.Sprintf("the node takes miner %d", j)
			take(step, what, miners[j])
		case r < 7: // another miner catches up
			i, j := 1+w.rng.IntN(len(miners)-1), w.rng.IntN(len(miners))
			if i == j {
				continue
			}
			_ = offer(miners[i], miners[j])
			what = fmt.Sprintf("miner %d takes miner %d", i, j)
		case r < 8 && deep < 2: // a branch from below the node's finality, with more work
			h, _, ok := subject.finality.Finalized()
			_, top, _ := subject.chain.Tip()
			if !ok || h < 5 {
				continue
			}
			at := h - 1 - uint64(w.rng.IntN(4))
			b := w.branch(subject, at)
			w.mine(b, int(top-at)+2)
			deep++
			what = fmt.Sprintf("a branch from %d, below finality at %d", at, h)
			take(step, what, b)
		case r < 9: // a restart
			h, id, ok := subject.finality.Finalized()
			subject.Close()
			subject = open()
			miners[0] = subject
			restarts++
			if h2, id2, ok2 := subject.finality.Finalized(); ok2 != ok || h2 != h || id2 != id {
				t.Fatalf("step %d: final at %d (%v) before the restart, at %d (%v) after", step, h, ok, h2, ok2)
			}
			what = "restart"
		default: // everybody hears every attestation
			for _, m := range miners[1:] {
				w.deliver(m)
			}
			what = "gossip"
		}
		w.deliver(subject)
		check(step, what)
	}

	// Every attestation there is, on the node and on a node that synced its chain from scratch.
	w.deliver(subject)
	check(steps, "the end")
	fresh := NewNode(chain.RegTest, chain.RegtestGenesis(), mh.TestProfile)
	defer fresh.Close()
	fresh.noAttest = true
	feedAll(t, fresh, subject)
	w.deliver(fresh)
	h, id, ok := subject.finality.Finalized()
	fh, fid, fok := fresh.finality.Finalized()
	if fok != ok || fh != h || fid != id {
		t.Fatalf("the node is final at %d (%v), a clean sync of its chain at %d (%v)", h, ok, fh, fok)
	}
	for i, m := range append(miners, fresh) {
		if c := m.finality.Caught(); len(c) > 0 {
			t.Fatalf("node %d caught an honest key signing twice: %v", i, c[0])
		}
	}
	_, top, _ := subject.chain.Tip()
	t.Logf("seed %d: %d steps, %d tip changes, %d branches from below finality, %d refused, %d restarts, height %d, final at %d, %d attestations",
		seed, steps, switches, deep, refused, restarts, top, h, len(w.atts))
	// The short run, and the one under the race detector, is about what runs at once, not coverage.
	if !testing.Short() && !raceEnabled && (!ok || switches < 3 || refused == 0 || h <= startH) {
		t.Fatalf("the scenario says too little: final at %d (%v, %d before the steps), %d tip changes, %d refused", h, ok, startH, switches, refused)
	}
}
