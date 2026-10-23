package blocksync

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"powmachina/internal/chain"
)

// The HTTP source and the HTTP server for block bodies.
//
// # Why HTTP and not an extension of P2P
//
// Blocks are **immutable and content-addressed**, so they are perfectly cacheable. Over HTTP anything
// can serve them — a node, nginx, S3, R2, GitHub Pages, a CDN — and the client verifies them itself
// anyway. That means **anybody can mirror the chain, without coordination and without permission**.
// P2P will never have that property.
//
// # What this is NOT
//
// It is not a trustworthy source of truth and must not be used as one. Speed always has to come from
// bandwidth, **never from skipped verification**. Whoever stopped checking for the sake of speed would
// turn a server into an authority over the chain — and that is exactly what we are avoiding.

// BlockPath is the path a block body is served at. The hex id is lower case.
const BlockPath = "/b/"

// HTTPSource pulls bodies from an HTTP endpoint. `Base` is for instance "https://blocks.example.com".
type HTTPSource struct {
	Base   string
	Client *http.Client
	// Parallel is how many blocks of one batch to pull at once. HTTP/1.1 connections can only be
	// pipelined to a limited extent, so parallel requests are the main source of speed here.
	Parallel int
}

func NewHTTPSource(base string) *HTTPSource {
	return &HTTPSource{
		Base:     strings.TrimRight(base, "/"),
		Client:   &http.Client{Timeout: 30 * time.Second},
		Parallel: 8,
	}
}

func (h *HTTPSource) Name() string { return h.Base }

func (h *HTTPSource) Fetch(ctx context.Context, ids [][32]byte) ([]*chain.Block, error) {
	par := h.Parallel
	if par <= 0 {
		par = 8
	}
	type result struct {
		b   *chain.Block
		err error
	}
	sem := make(chan struct{}, par)
	results := make([]result, len(ids))
	var wg sync.WaitGroup

	for i, id := range ids {
		wg.Add(1)
		go func(i int, id [32]byte) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			b, err := h.one(ctx, id)
			results[i] = result{b, err}
		}(i, id)
	}
	wg.Wait()

	out := make([]*chain.Block, 0, len(ids))
	var firstErr error
	for _, r := range results {
		if r.err != nil {
			if firstErr == nil {
				firstErr = r.err
			}
			continue // a missing block is not a lie, another source will take it
		}
		out = append(out, r.b)
	}
	if len(out) == 0 && firstErr != nil {
		return nil, firstErr
	}
	return out, nil
}

func (h *HTTPSource) one(ctx context.Context, id [32]byte) (*chain.Block, error) {
	url := h.Base + BlockPath + hex.EncodeToString(id[:])
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := h.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("blocksync: %s returned %s", url, resp.Status)
	}
	// A size cap: without it a source could send an endless stream.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, int64(chain.MaxBlockWireBytes)+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > chain.MaxBlockWireBytes {
		return nil, fmt.Errorf("%w: %s sent more than MaxBlockWireBytes", ErrBadBlock, h.Base)
	}
	return chain.DeserializeBlock(raw)
}

// BlockReader is what the server needs for serving — typically `*store.Store`.
type BlockReader interface {
	Block(id [32]byte) (*chain.Block, error)
}

// Handler serves `GET /b/<hexid>` and returns the serialized block body.
//
// It is an **opt-in** feature of the node and it has to have a transfer cap: an open endpoint that pours
// out gigabytes on request is a DoS amplifier. The limits are the caller's job (a reverse proxy or
// `LimitedHandler`).
func Handler(r BlockReader) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet && req.Method != http.MethodHead {
			http.Error(w, "GET only", http.StatusMethodNotAllowed)
			return
		}
		h := strings.TrimPrefix(req.URL.Path, BlockPath)
		raw, err := hex.DecodeString(h)
		if err != nil || len(raw) != 32 {
			http.Error(w, "a bad block id", http.StatusBadRequest)
			return
		}
		var id [32]byte
		copy(id[:], raw)
		b, err := r.Block(id)
		if err != nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		body := b.Serialize()
		// Blocks are immutable, so they can be cached forever. That is the whole reason an ordinary CDN
		// is enough to put in front of this.
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		if req.Method == http.MethodHead {
			return
		}
		_, _ = w.Write(body)
	})
}

// LimitedHandler wraps Handler with a cap on the number of concurrent requests and on the bytes sent.
// Without a cap nobody would turn this on twice.
func LimitedHandler(r BlockReader, maxConcurrent int, byteBudget int64) http.Handler {
	inner := Handler(r)
	sem := make(chan struct{}, maxConcurrent)
	var spent atomic64
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if byteBudget > 0 && spent.load() >= byteBudget {
			http.Error(w, "the transfer budget is exhausted", http.StatusTooManyRequests)
			return
		}
		select {
		case sem <- struct{}{}:
			defer func() { <-sem }()
		default:
			http.Error(w, "too many concurrent requests", http.StatusTooManyRequests)
			return
		}
		cw := &countingWriter{ResponseWriter: w}
		inner.ServeHTTP(cw, req)
		spent.add(cw.n)
	})
}

type countingWriter struct {
	http.ResponseWriter
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.ResponseWriter.Write(p)
	c.n += int64(n)
	return n, err
}

// atomic64 is a small wrapper so the budget is safe across requests.
type atomic64 struct {
	mu sync.Mutex
	v  int64
}

func (a *atomic64) add(n int64) {
	a.mu.Lock()
	a.v += n
	a.mu.Unlock()
}

func (a *atomic64) load() int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.v
}
