package chain

import (
	"crypto/ed25519"
	"errors"
	"math"
	"slices"
	"sync"
	"testing"
	"time"

	"powmachina/internal/mh"
)

// finalityKey is a deterministic finality key for a test.
func finalityKey(i int) ed25519.PrivateKey {
	seed := make([]byte, ed25519.SeedSize)
	copy(seed, "powmachina-test-finality-key")
	seed[ed25519.SeedSize-1] = byte(i)
	return ed25519.NewKeyFromSeed(seed)
}

func pubOf(priv ed25519.PrivateKey) [32]byte {
	var k [32]byte
	copy(k[:], priv.Public().(ed25519.PublicKey))
	return k
}

func blockID(b byte) [32]byte {
	var id [32]byte
	id[0] = b
	id[31] = 0x5a
	return id
}

// window is a chain as the quorum sees it: the seat of every height. Lanes take turns.
type window map[uint64]Seat

// mined gives heights from..to (inclusive) to a key; nil = a block without a key.
func (w window) mined(from, to uint64, key ed25519.PrivateKey) window {
	for h := from; h <= to; h++ {
		s := Seat{Lane: mh.Lane(h % 2)}
		if key != nil {
			s.Key, s.HasKey = pubOf(key), true
		}
		w[h] = s
	}
	return w
}

func (w window) lookup(h uint64) (Seat, bool) {
	s, ok := w[h]
	return s, ok
}

// The chain the node really has.
func chainOf(ids map[uint64][32]byte) ChainLookup {
	return func(h uint64) ([32]byte, bool) {
		id, ok := ids[h]
		return id, ok
	}
}

const testWindow = 30

// quorumsOf is the QuorumLookup of a test window.
func quorumsOf(w window) QuorumLookup {
	return func(h uint64) *Quorum { return QuorumFor(h, testWindow, w.lookup) }
}

// The key travels in the share commitment, under the block's PoW: a key changed in a copy of the
// block no longer matches the header.
func TestFinalityKeyRidesInTheShareCommitment(t *testing.T) {
	key := pubOf(finalityKey(1))

	u, b := sealedSpend(t, 10, RegTest)
	if _, ok := b.FinalityKey(); ok {
		t.Fatal("a block without a key has one")
	}
	b.Share.FinalityKey = key
	b.Seal()
	if got, ok := b.FinalityKey(); !ok || got != key {
		t.Fatalf("key %x (%v)", got[:4], ok)
	}
	if err := b.ValidateBlock(u, RegTest); err != nil {
		t.Fatalf("a block with a key: %v", err)
	}

	c := copyOf(t, b)
	c.Share.FinalityKey = pubOf(finalityKey(2))
	if err := c.CheckRoot(); !errors.Is(err, ErrBadMerkle) {
		t.Fatalf("a copy with another key: %v, expected ErrBadMerkle", err)
	}
}

// The witness commitment is the whole data output: nothing may follow it.
func TestWitnessCommitmentTakesNothingAfterIt(t *testing.T) {
	_, b := sealedSpend(t, 10, RegTest)
	data := &b.Txs[0].Outputs[len(b.Txs[0].Outputs)-1]
	for _, tail := range [][]byte{{1}, append([]byte("PMfk"), make([]byte, 32)...)} {
		c := copyOf(t, b)
		cd := &c.Txs[0].Outputs[len(c.Txs[0].Outputs)-1]
		cd.Script = append(append([]byte{}, data.Script...), tail...)
		if _, err := c.witnessCommitment(); !errors.Is(err, ErrNoWitnessCommitment) {
			t.Fatalf("a tail of %d bytes: %v", len(tail), err)
		}
	}
}

// No quorum before the window is full: the first miners of a chain would otherwise be all of it.
func TestQuorumNeedsAFullWindow(t *testing.T) {
	w := window{}.mined(0, 0, nil).mined(1, 100, finalityKey(1))
	if q := QuorumFor(testWindow, testWindow, w.lookup); q != nil {
		t.Fatal("a quorum over a window that reaches the genesis")
	}
	if q := QuorumFor(testWindow+1, testWindow, w.lookup); q == nil {
		t.Fatal("no quorum over a full window")
	}
	if q := QuorumFor(200, 0, w.lookup); q != nil {
		t.Fatal("a quorum on a network without finality")
	}
	// A height of the window that is not here yet means no quorum, not a smaller one.
	delete(w, 50)
	if q := QuorumFor(60, testWindow, w.lookup); q != nil {
		t.Fatal("a quorum over a window with a hole")
	}
}

// Each lane holds half of the weight. A miner who has every block of one lane has half, however
// many blocks that lane had.
func TestQuorumWeighsTheLanesEqually(t *testing.T) {
	gpu, cpu1, cpu2 := finalityKey(1), finalityKey(2), finalityKey(3)
	w := window{}
	for h := uint64(1); h <= 100; h++ {
		s := Seat{Lane: mh.LaneG, Key: pubOf(gpu), HasKey: true}
		switch {
		case h%3 == 0: // a third of the blocks are lane C
			s = Seat{Lane: mh.LaneC, Key: pubOf(cpu1), HasKey: true}
			if h%2 == 0 {
				s.Key = pubOf(cpu2)
			}
		}
		w[h] = s
	}
	q := QuorumFor(91, testWindow, w.lookup)
	if q.Weight(pubOf(gpu))*2 != q.Total() {
		t.Fatalf("all of lane G weighs %d of %d", q.Weight(pubOf(gpu)), q.Total())
	}
	if got := q.Weight(pubOf(cpu1)) + q.Weight(pubOf(cpu2)); got*2 != q.Total() {
		t.Fatalf("all of lane C weighs %d of %d", got, q.Total())
	}
	if q.Members() != 3 {
		t.Fatalf("%d members", q.Members())
	}
	// Half is not more than two thirds — one lane alone never finalizes.
	if q.Enough(q.Weight(pubOf(gpu))) {
		t.Fatal("lane G alone finalized")
	}
}

// A block without a key weighs like any other and nobody signs for it.
func TestKeylessBlocksCountAgainst(t *testing.T) {
	a := finalityKey(1)
	w := window{}.mined(1, 20, a).mined(21, 30, nil)
	q := QuorumFor(31, testWindow, w.lookup)
	if q.Members() != 1 || q.Weight(pubOf(a))*3 != q.Total()*2 {
		t.Fatalf("two thirds with a key: weight %d of %d", q.Weight(pubOf(a)), q.Total())
	}
	if q.Enough(q.Weight(pubOf(a))) {
		t.Fatal("exactly two thirds finalized — it takes more")
	}
}

// signAll has every key sign the block at a height and files it.
func signAll(t *testing.T, f *FinalityState, q *Quorum, height uint64, id [32]byte, keys ...ed25519.PrivateKey) {
	t.Helper()
	for _, k := range keys {
		if _, err := f.Add(Sign(k, height, id), q); err != nil {
			t.Fatalf("key %x at %d: %v", k.Public(), height, err)
		}
	}
}

// More than two thirds of the weight finalizes, two thirds or less does not.
func TestFinalityTakesMoreThanTwoThirds(t *testing.T) {
	a, b, c := finalityKey(1), finalityKey(2), finalityKey(3)
	// 40 blocks: a has 20, b 10, c 10 — a+b = 75 %, a alone 50 %, b+c 50 %.
	w := window{}.mined(1, 20, a).mined(21, 30, b).mined(31, 40, c)
	const h = 40
	ids := map[uint64][32]byte{h: blockID(0xaa)}
	q := quorumsOf(w)(h)

	f := NewFinalityState()
	signAll(t, f, q, h, ids[h], b, c)
	f.Update(h+FinalityDepth, chainOf(ids), quorumsOf(w))
	if _, _, ok := f.Finalized(); ok {
		t.Fatal("half of the weight finalized")
	}
	signAll(t, f, q, h, ids[h], a)
	f.Update(h+FinalityDepth, chainOf(ids), quorumsOf(w))
	if got, id, ok := f.Finalized(); !ok || got != h || id != ids[h] {
		t.Fatalf("finality %d (%v), expected %d", got, ok, h)
	}
}

// Only a block deep enough below the tip may be finalized.
func TestFinalityRespectsDepth(t *testing.T) {
	a := finalityKey(1)
	w := window{}.mined(1, 100, a)
	ids := map[uint64][32]byte{50: blockID(0x50)}
	f := NewFinalityState()
	signAll(t, f, quorumsOf(w)(50), 50, ids[50], a)
	f.Update(50+FinalityDepth-1, chainOf(ids), quorumsOf(w))
	if _, _, ok := f.Finalized(); ok {
		t.Fatal("a block shallower than the depth was finalized")
	}
	f.Update(50+FinalityDepth, chainOf(ids), quorumsOf(w))
	if got, _, ok := f.Finalized(); !ok || got != 50 {
		t.Fatalf("finality %d (%v)", got, ok)
	}
}

// This is the core of the whole design: the quorum cannot finalize a block the node does not have
// on its chain — not even unanimously.
func TestQuorumCannotFinalizeForeignBlock(t *testing.T) {
	a, b := finalityKey(1), finalityKey(2)
	w := window{}.mined(1, 30, a).mined(31, 60, b)
	ours := map[uint64][32]byte{55: blockID(0x01)}
	f := NewFinalityState()
	signAll(t, f, quorumsOf(w)(55), 55, blockID(0xEE), a, b)
	f.Update(200, chainOf(ours), quorumsOf(w))
	if _, _, ok := f.Finalized(); ok {
		t.Fatal("a unanimous quorum finalized a block the node does not have")
	}
}

// A key without a seat in the quorum of that height is turned away — it is not stored and not
// relayed, so attestations cannot be made up by the thousand.
func TestKeyWithoutASeatIsTurnedAway(t *testing.T) {
	a, stranger := finalityKey(1), finalityKey(9)
	w := window{}.mined(1, 60, a)
	f := NewFinalityState()
	q := quorumsOf(w)(50)
	if _, err := f.Add(Sign(stranger, 50, blockID(1)), q); !errors.Is(err, ErrUnknownSigner) {
		t.Fatalf("a key without a seat: %v", err)
	}
	if _, err := f.Add(Sign(a, 50, blockID(1)), nil); !errors.Is(err, ErrUnknownSigner) {
		t.Fatalf("no quorum for the height: %v", err)
	}
	if _, err := f.Add(Sign(a, 51, blockID(1)), q); !errors.Is(err, ErrUnknownSigner) {
		t.Fatalf("the quorum of another height: %v", err)
	}
	bad := Sign(a, 50, blockID(1))
	bad.Signature[3] ^= 1
	if _, err := f.Add(bad, q); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("a broken signature: %v", err)
	}
	if len(f.byHeight) != 0 {
		t.Fatal("something was stored")
	}
}

// A key that signs two blocks at one height is caught. Its signatures stop counting everywhere,
// its weight stays in the quorum — a caught key can only make finality harder.
func TestACaughtKeyStopsCounting(t *testing.T) {
	a, b := finalityKey(1), finalityKey(2)
	w := window{}.mined(1, 40, a).mined(41, 50, b) // a has 80 %
	f := NewFinalityState()
	q := quorumsOf(w)(50)
	if _, err := f.Add(Sign(a, 50, blockID(0xAA)), q); err != nil {
		t.Fatal(err)
	}
	ev, err := f.Add(Sign(a, 50, blockID(0xBB)), q)
	if ev == nil || !errors.Is(err, ErrSignerConflict) || ev.Verify() != nil {
		t.Fatalf("the conflict was not recognized: %v, %v", ev, err)
	}
	// Its signature at another height counts for nothing now.
	ids := map[uint64][32]byte{60: blockID(0x60)}
	w.mined(51, 60, a)
	signAll(t, f, quorumsOf(w)(60), 60, ids[60], a, b)
	f.Update(60+FinalityDepth, chainOf(ids), quorumsOf(w))
	if _, _, ok := f.Finalized(); ok {
		t.Fatal("a caught key still finalized")
	}
	if len(f.Caught()) != 1 {
		t.Fatalf("%d caught keys", len(f.Caught()))
	}
}

// A proof from the network has to be one: the same key, the same height, two blocks, two
// signatures — and a key with a seat in that height's quorum, or anybody could fill the node's
// memory and disk with keys made up to sign twice (PM-03).
func TestEquivocationFromTheNetworkIsChecked(t *testing.T) {
	a, b, stranger := finalityKey(1), finalityKey(2), finalityKey(3)
	q := quorumsOf(window{}.mined(1, 40, a).mined(41, 50, b))(50)
	good := &Equivocation{Key: pubOf(a), Height: 50, First: Sign(a, 50, blockID(1)), Second: Sign(a, 50, blockID(2))}
	f := NewFinalityState()
	if !f.AddEquivocation(good, q) {
		t.Fatal("a valid proof was refused")
	}
	if f.AddEquivocation(good, q) {
		t.Fatal("the same key was caught twice")
	}
	for name, e := range map[string]*Equivocation{
		"two keys":      {Key: pubOf(b), Height: 50, First: Sign(b, 50, blockID(1)), Second: Sign(a, 50, blockID(2))},
		"two heights":   {Key: pubOf(b), Height: 50, First: Sign(b, 50, blockID(1)), Second: Sign(b, 51, blockID(2))},
		"one block":     {Key: pubOf(b), Height: 50, First: Sign(b, 50, blockID(1)), Second: Sign(b, 50, blockID(1))},
		"a forged half": {Key: pubOf(b), Height: 50, First: Sign(b, 50, blockID(1)), Second: Attestation{Height: 50, BlockID: blockID(2), Key: pubOf(b), Signature: make([]byte, 64)}},
		"no seat":       {Key: pubOf(stranger), Height: 50, First: Sign(stranger, 50, blockID(1)), Second: Sign(stranger, 50, blockID(2))},
	} {
		if f.AddEquivocation(e, q) {
			t.Fatalf("%s: accepted", name)
		}
	}
	other := &Equivocation{Key: pubOf(b), Height: 50, First: Sign(b, 50, blockID(1)), Second: Sign(b, 50, blockID(2))}
	if f.AddEquivocation(other, nil) {
		t.Fatal("accepted without a quorum to check the key against")
	}
	if f.AddEquivocation(&Equivocation{Key: pubOf(b), Height: 49, First: Sign(b, 49, blockID(1)), Second: Sign(b, 49, blockID(2))}, q) {
		t.Fatal("accepted against the quorum of another height")
	}
	if len(f.Caught()) != 1 {
		t.Fatalf("%d caught keys", len(f.Caught()))
	}
}

// An attestation we already have has to be refused by Add as a duplicate — that is how the node
// knows **not to relay it**. Without that, gossip between three or more nodes loops forever and
// floods the peers.
func TestAddRejectsDuplicateSoGossipStops(t *testing.T) {
	a := finalityKey(1)
	w := window{}.mined(1, 200, a)
	f := NewFinalityState()
	q := quorumsOf(w)(100)
	att := Sign(a, 100, blockID(3))
	if ev, err := f.Add(att, q); err != nil || ev != nil {
		t.Fatalf("the first attestation should have passed: %v, %v", ev, err)
	}
	for i := 0; i < 5; i++ {
		ev, err := f.Add(att, q)
		if !errors.Is(err, ErrDuplicateAttestation) || ev != nil {
			t.Fatalf("repeat %d: %v, %v", i, ev, err)
		}
	}
	if n := len(f.byHeight[100]); n != 1 {
		t.Fatalf("the duplicates were counted: %d attestations", n)
	}
	// A conflicting one is proof once; its repeats are duplicates, so not even the proof travels in circles.
	second := Sign(a, 100, blockID(4))
	if ev, err := f.Add(second, q); ev == nil || !errors.Is(err, ErrSignerConflict) {
		t.Fatalf("the conflict: %v, %v", ev, err)
	}
	if ev, err := f.Add(second, q); ev != nil || !errors.Is(err, ErrDuplicateAttestation) {
		t.Fatalf("a repeated conflict: %v, %v", ev, err)
	}
}

// The state must not grow with the chain: what is below the finalized height is never recomputed.
func TestFinalityStatePrunesBelowFinalized(t *testing.T) {
	a := finalityKey(1)
	w := window{}.mined(1, 100, a)
	const target = 70
	ids := map[uint64][32]byte{}
	f := NewFinalityState()
	for h := uint64(60); h <= target; h++ {
		ids[h] = blockID(byte(h))
		signAll(t, f, quorumsOf(w)(h), h, ids[h], a)
	}
	f.Update(target+FinalityDepth, chainOf(ids), quorumsOf(w))
	if got, _, ok := f.Finalized(); !ok || got != target {
		t.Fatalf("finality is %d (%v), expected %d", got, ok, target)
	}
	for h := range f.byHeight {
		if h < target {
			t.Fatalf("height %d stayed in the state below the finalized %d", h, target)
		}
	}
	for k := range f.seen {
		if k.height < target {
			t.Fatalf("the attestation of height %d stayed in the seen list below %d", k.height, target)
		}
	}
	// And a late attestation below it is a duplicate, not something new to relay.
	if _, err := f.Add(Sign(a, 65, ids[65]), quorumsOf(w)(65)); !errors.Is(err, ErrDuplicateAttestation) {
		t.Fatalf("an attestation below the finalized height: %v", err)
	}
}

// Several goroutines push into the finality state at once: every peer's read loop and the node's
// own signer. Without a lock that ends as a crash on the map.
func TestFinalityStateConcurrentAdd(t *testing.T) {
	keys := []ed25519.PrivateKey{finalityKey(1), finalityKey(2), finalityKey(3), finalityKey(4)}
	w := window{}
	for h := uint64(1); h <= 100; h++ {
		w[h] = Seat{Lane: mh.Lane(h % 2), Key: pubOf(keys[h%4]), HasKey: true}
	}
	ids := map[uint64][32]byte{}
	for h := uint64(40); h <= 70; h++ {
		ids[h] = blockID(byte(h))
	}
	f0 := NewFinalityState()
	var wg sync.WaitGroup
	for _, k := range keys {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for h := uint64(40); h <= 70; h++ {
				att := Sign(k, h, ids[h])
				_, _ = f0.Add(att, quorumsOf(w)(h))
				_, _ = f0.Add(att, quorumsOf(w)(h))
				f0.Update(70+FinalityDepth, chainOf(ids), quorumsOf(w))
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range 200 {
			_, _, _ = f0.Finalized()
			_ = f0.CheckReorg(60)
		}
	}()
	wg.Wait()
	f0.Update(70+FinalityDepth, chainOf(ids), quorumsOf(w))
	if got, id, ok := f0.Finalized(); !ok || got != 70 || id != ids[70] {
		t.Fatalf("finality is %d (%v), expected 70", got, ok)
	}
}

// A reorg below a finalized block is refused, above it it is an ordinary thing.
func TestReorgGuard(t *testing.T) {
	a := finalityKey(1)
	w := window{}.mined(1, 200, a)
	ids := map[uint64][32]byte{100: blockID(0xaa)}
	f := NewFinalityState()
	signAll(t, f, quorumsOf(w)(100), 100, ids[100], a)
	f.Update(200, chainOf(ids), quorumsOf(w))

	if err := f.CheckReorg(99); err != ErrFinalityRewrite {
		t.Fatalf("a reorg below a finalized block should be refused, got %v", err)
	}
	if err := f.CheckReorg(100); err != nil {
		t.Fatalf("a reorg from the finalized height upwards should pass, got %v", err)
	}
	if err := NewFinalityState().CheckReorg(1); err != nil {
		t.Fatalf("without attestations nothing may be blocked, got %v", err)
	}
}

// What a node recorded before it stopped comes back, and only moves finality forward.
func TestRestoreOnlyMovesForward(t *testing.T) {
	f := NewFinalityState()
	f.Restore(100, blockID(1))
	if h, id, ok := f.Finalized(); !ok || h != 100 || id != blockID(1) {
		t.Fatalf("restored %d %x %v", h, id[:2], ok)
	}
	f.Restore(90, blockID(2))
	if h, _, _ := f.Finalized(); h != 100 {
		t.Fatalf("an older record moved finality back to %d", h)
	}
	if err := f.CheckReorg(99); err != ErrFinalityRewrite {
		t.Fatalf("a reorg below a restored finality: %v", err)
	}
}

// The range of a getattest request is the peer's. A range that ends at the last possible height, or
// covers every height there is, must cost no more than the attestations the node holds.
func TestInRangeDoesNotWalkTheRange(t *testing.T) {
	a, b := finalityKey(1), finalityKey(2)
	w := window{}
	for h := uint64(1); h <= 60; h++ {
		k := a
		if h%2 == 0 {
			k = b
		}
		w.mined(h, h, k)
	}
	f := NewFinalityState()
	for _, h := range []uint64{47, 45, 46} {
		signAll(t, f, quorumsOf(w)(h), h, blockID(byte(h)), a, b)
	}
	type span struct {
		from, to uint64
		want     []uint64
	}
	spans := []span{
		{math.MaxUint64, math.MaxUint64, nil},
		{0, math.MaxUint64, []uint64{45, 45, 46, 46, 47, 47}},
		{46, math.MaxUint64, []uint64{46, 46, 47, 47}},
		{46, 46, []uint64{46, 46}},
		{48, 45, nil},
	}
	for _, sp := range spans {
		done := make(chan []Attestation, 1)
		go func() { done <- f.InRange(sp.from, sp.to, 640) }()
		select {
		case got := <-done:
			var heights []uint64
			for _, a := range got {
				heights = append(heights, a.Height)
			}
			if !slices.Equal(heights, sp.want) {
				t.Fatalf("InRange(%d, %d) gave heights %v, want %v", sp.from, sp.to, heights, sp.want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("InRange(%d, %d) is still walking", sp.from, sp.to)
		}
	}
	if got := f.InRange(0, math.MaxUint64, 3); len(got) != 3 {
		t.Fatalf("the cap of 3 gave %d", len(got))
	}
}
