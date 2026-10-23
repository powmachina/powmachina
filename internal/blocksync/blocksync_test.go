package blocksync

import (
	"context"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"powmachina/internal/chain"
)

func buildChain(n int) []*chain.Block {
	out := make([]*chain.Block, 0, n)
	var prev [32]byte
	for h := 0; h < n; h++ {
		var extra [32]byte
		extra[0] = byte(h)
		extra[1] = byte(h >> 8)
		key := make([]byte, 32)
		key[0] = byte(h % 5)
		b := &chain.Block{
			Header: &chain.Header{
				Version: 1, PrevBlock: prev, Bits: chain.RegtestTargetBits,
				Height: uint32(h), Time: uint32(1_700_000_000 + h*120), Lane: 1,
			},
			Txs: []*chain.Tx{chain.BuildCoinbase(uint64(h), nil, extra, chain.RegTest)},
		}
		b.Seal()
		prev = b.Header.ID()
		out = append(out, b)
	}
	return out
}

func jobsFor(blocks []*chain.Block) []Job {
	js := make([]Job, len(blocks))
	for i, b := range blocks {
		js[i] = Job{Height: uint64(b.Header.Height), ID: b.Header.ID()}
	}
	return js
}

// memSink is a Sink that remembers what it received.
type memSink struct {
	mu   sync.Mutex
	got  map[[32]byte]*chain.Block
	fail error
}

func newSink() *memSink { return &memSink{got: map[[32]byte]*chain.Block{}} }

func (m *memSink) PutBlock(b *chain.Block) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return m.fail
	}
	m.got[b.Header.ID()] = b
	return nil
}

func (m *memSink) len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.got)
}

// honest is an ordinary source with an optional delay.
type honest struct {
	name  string
	byID  map[[32]byte]*chain.Block
	delay time.Duration
	calls atomic.Int64
}

func newHonest(name string, blocks []*chain.Block, delay time.Duration) *honest {
	m := map[[32]byte]*chain.Block{}
	for _, b := range blocks {
		m[b.Header.ID()] = b
	}
	return &honest{name: name, byID: m, delay: delay}
}

func (h *honest) Name() string { return h.name }
func (h *honest) Fetch(ctx context.Context, ids [][32]byte) ([]*chain.Block, error) {
	h.calls.Add(1)
	if h.delay > 0 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(h.delay):
		}
	}
	out := make([]*chain.Block, 0, len(ids))
	for _, id := range ids {
		if b, ok := h.byID[id]; ok {
			out = append(out, b)
		}
	}
	return out, nil
}

// Several sources split the work between them and the result is complete.
func TestParallelSourcesSplitWork(t *testing.T) {
	blocks := buildChain(200)
	srcs := []Source{
		newHonest("a", blocks, 2*time.Millisecond),
		newHonest("b", blocks, 2*time.Millisecond),
		newHonest("c", blocks, 2*time.Millisecond),
		newHonest("d", blocks, 2*time.Millisecond),
	}
	sink := newSink()
	d := New()
	d.BatchSize = 8

	st, err := d.Run(context.Background(), jobsFor(blocks), srcs, sink)
	if err != nil {
		t.Fatal(err)
	}
	if int(st.Blocks) != len(blocks) || sink.len() != len(blocks) {
		t.Fatalf("downloaded %d/%d blocks (sink %d)", st.Blocks, len(blocks), sink.len())
	}
	// Every source should have got work — otherwise there is no parallelism.
	used := 0
	for _, s := range srcs {
		if s.(*honest).calls.Load() > 0 {
			used++
		}
	}
	if used < 2 {
		t.Fatalf("only %d sources of %d got work", used, len(srcs))
	}
	t.Logf("%d blocks, %d B, %d/%d sources got work", st.Blocks, st.Bytes, used, len(srcs))
}

// A source that slips in a body with different transactions has to be caught even when the header
// matches. Without the merkle root check it would pass — and that is a silent catastrophe.
type swapsBody struct{ inner *honest }

func (s *swapsBody) Name() string { return "fraudster" }
func (s *swapsBody) Fetch(ctx context.Context, ids [][32]byte) ([]*chain.Block, error) {
	out, err := s.inner.Fetch(ctx, ids)
	if err != nil || len(out) == 0 {
		return out, err
	}
	// The same header, different transactions.
	victim := out[0]
	key := make([]byte, 32)
	key[0] = 0xAA
	var extra [32]byte
	extra[0] = 0xBB
	fake := &chain.Block{
		Header: victim.Header,
		Txs:    []*chain.Tx{chain.BuildCoinbase(uint64(victim.Header.Height), nil, extra, chain.RegTest)},
	}
	fake.Txs[0].Outputs = append(fake.Txs[0].Outputs, chain.TxOut{Value: 1, Script: key})
	res := append([]*chain.Block{fake}, out[1:]...)
	return res, nil
}

func TestRejectsSwappedBody(t *testing.T) {
	blocks := buildChain(40)
	bad := &swapsBody{inner: newHonest("source", blocks, 0)}
	// The honest source is deliberately slow, otherwise it would empty the queue before the fraudster got
	// any work — and the test would be random.
	good := newHonest("good", blocks, time.Millisecond)

	sink := newSink()
	d := New()
	d.BatchSize = 4

	st, err := d.Run(context.Background(), jobsFor(blocks), []Source{bad, good}, sink)
	if err != nil {
		t.Fatal(err)
	}
	if int(st.Blocks) != len(blocks) {
		t.Fatalf("everything should have been downloaded even with the fraudster: %d/%d", st.Blocks, len(blocks))
	}
	// The fraudster has to be thrown away and its version must not be in the sink.
	found := false
	for _, dn := range st.Dropped {
		if strings.Contains(dn, "fraudster") && strings.Contains(dn, "lied") {
			found = true
		}
	}
	// The eviction can only be expected when the fraudster really got work. Otherwise the test would
	// depend on timing — what matters is that its version must not end up in the sink.
	if calls := bad.inner.calls.Load(); calls > 0 && !found {
		t.Fatalf("the fraudster got %d batches and was not thrown away, thrown away: %v", calls, st.Dropped)
	}
	for _, b := range blocks {
		got := sink.got[b.Header.ID()]
		if got == nil {
			t.Fatalf("the block %d is missing", b.Header.Height)
		}
		if got.CheckRoot() != nil {
			t.Fatalf("the block %d has a planted body in the sink", b.Header.Height)
		}
	}
	t.Logf("the fraudster was caught and thrown away, %d blocks downloaded from the rest", st.Blocks)
}

// A source that always fails must not block the sync — the others take over the work.
type broken struct {
	name  string
	calls atomic.Int64
}

func (b *broken) Name() string { return b.name }
func (b *broken) Fetch(context.Context, [][32]byte) ([]*chain.Block, error) {
	b.calls.Add(1)
	return nil, errors.New("the connection failed")
}

func TestBrokenSourceIsDroppedAndWorkContinues(t *testing.T) {
	blocks := buildChain(60)
	sink := newSink()
	d := New()
	d.BatchSize = 4

	bad := &broken{name: "dead"}
	// The healthy source is deliberately slow, otherwise it would empty the queue before the broken one
	// used up its attempts — and the test would be random.
	st, err := d.Run(context.Background(),
		jobsFor(blocks), []Source{bad, newHonest("alive", blocks, time.Millisecond)}, sink)
	if err != nil {
		t.Fatal(err)
	}

	// The essential property: a broken source **does not stop** the sync.
	if int(st.Blocks) != len(blocks) {
		t.Fatalf("everything should have been downloaded: %d/%d", st.Blocks, len(blocks))
	}
	// The eviction is an optimization — it can only be expected when it really managed to fail that many
	// times. Otherwise the test would depend on timing.
	if bad.calls.Load() >= int64(d.MaxFailures) && len(st.Dropped) == 0 {
		t.Fatalf("the source failed %d times and was not thrown away", bad.calls.Load())
	}
	t.Logf("the broken source: %d attempts, thrown away: %v", bad.calls.Load(), st.Dropped)
}

// When every source fails, it has to end with an error rather than freeze.
func TestAllSourcesFailTerminates(t *testing.T) {
	blocks := buildChain(20)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := New().Run(ctx, jobsFor(blocks), []Source{&broken{name: "a"}, &broken{name: "b"}}, newSink())
	if !errors.Is(err, ErrNoSources) {
		t.Fatalf("expected ErrNoSources, got %v", err)
	}
}

// A source that delivers only part of a batch is not a fraudster — the others take the rest.
type partial struct{ inner *honest }

func (p *partial) Name() string { return "halfway" }
func (p *partial) Fetch(ctx context.Context, ids [][32]byte) ([]*chain.Block, error) {
	out, err := p.inner.Fetch(ctx, ids)
	if err != nil || len(out) < 2 {
		return out, err
	}
	return out[:len(out)/2], nil
}

func TestPartialDeliveryIsNotFatal(t *testing.T) {
	blocks := buildChain(80)
	sink := newSink()
	d := New()
	d.BatchSize = 8

	st, err := d.Run(context.Background(),
		jobsFor(blocks), []Source{&partial{newHonest("p", blocks, 0)}, newHonest("full", blocks, 0)}, sink)
	if err != nil {
		t.Fatal(err)
	}
	if int(st.Blocks) != len(blocks) {
		t.Fatalf("everything should have been downloaded: %d/%d", st.Blocks, len(blocks))
	}
}

// ---------------------------------------------------------------- HTTP

type mapReader struct{ byID map[[32]byte]*chain.Block }

func (m mapReader) Block(id [32]byte) (*chain.Block, error) {
	b, ok := m.byID[id]
	if !ok {
		return nil, errors.New("not found")
	}
	return b, nil
}

// The whole path through a real HTTP server: the node serves, the client pulls and verifies.
func TestHTTPSourceEndToEnd(t *testing.T) {
	blocks := buildChain(50)
	byID := map[[32]byte]*chain.Block{}
	for _, b := range blocks {
		byID[b.Header.ID()] = b
	}
	srv := httptest.NewServer(LimitedHandler(mapReader{byID}, 8, 0))
	defer srv.Close()

	sink := newSink()
	d := New()
	d.BatchSize = 10

	st, err := d.Run(context.Background(), jobsFor(blocks),
		[]Source{NewHTTPSource(srv.URL)}, sink)
	if err != nil {
		t.Fatal(err)
	}
	if int(st.Blocks) != len(blocks) {
		t.Fatalf("everything should have been downloaded over HTTP: %d/%d", st.Blocks, len(blocks))
	}
	t.Logf("HTTP: %d blocks, %d B", st.Blocks, st.Bytes)
}

// Blocks are immutable, so the answer has to be cacheable — the fact that an ordinary CDN is enough to
// put in front of this stands on that.
func TestHTTPResponseIsCacheable(t *testing.T) {
	blocks := buildChain(1)
	byID := map[[32]byte]*chain.Block{blocks[0].Header.ID(): blocks[0]}
	srv := httptest.NewServer(Handler(mapReader{byID}))
	defer srv.Close()

	id := blocks[0].Header.ID()
	resp, err := http.Get(srv.URL + BlockPath + hex.EncodeToString(id[:]))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if cc := resp.Header.Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Fatalf("the answer has to be cacheable, got %q", cc)
	}
}

// A server serving a FOREIGN chain must not be able to fool the client — the hash does not match.
func TestHTTPSourceServingWrongChainIsRejected(t *testing.T) {
	ours := buildChain(20)
	theirs := buildChain(20)
	// A different chain: we change the time, so the ids will be different.
	for _, b := range theirs {
		b.Header.Time += 7
	}
	byID := map[[32]byte]*chain.Block{}
	for _, b := range theirs {
		byID[b.Header.ID()] = b
	}
	srv := httptest.NewServer(Handler(mapReader{byID}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := New().Run(ctx, jobsFor(ours), []Source{NewHTTPSource(srv.URL)}, newSink())
	if !errors.Is(err, ErrNoSources) {
		t.Fatalf("a server with a foreign chain should deliver nothing, got %v", err)
	}
}
