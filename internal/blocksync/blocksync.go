// Package blocksync downloads block bodies from several sources at once.
//
// # Why this is not just "faster P2P"
//
// A sync tends to be slow even on a gigabit link, and not because of bandwidth. The reasons are: a peer
// has a weak upload, there are few sources (8 outbound connections), the request window is small and one
// slow peer stalls the whole window. The bottleneck is the **number and quality of sources**, not the
// client's link.
//
// # What this makes possible
//
// Headers are 89 B, so a year of chain is 22 MiB and downloads in seconds. Once they are verified, **we
// know the hash of every block we do not have yet**.
//
// The header chain is therefore a verified manifest — exactly what a `.torrent` file is for BitTorrent.
// And from that follows the property this whole package stands on:
//
//	A block's body can be downloaded FROM ANYBODY. Even from an enemy.
//
// A bad source cannot slip anything in — either it gives bytes whose hash matches, or we throw it away.
// That is why a source does not have to be trustworthy and does not even have to be a node: static
// hosting is enough.
//
// # What is verified for every block
//
//  1. `Header.ID()` has to equal the id from the manifest (otherwise it is not that block),
//  2. `TxMerkleRoot(Txs)` has to equal `Header.MerkleRoot`.
//
// Point 2 is easy to forget and it is fatal: without it a source could send a correct header with
// completely different transactions.
package blocksync

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"powmachina/internal/chain"
)

// Job is one block to download — its height and its **already verified** hash from the manifest.
type Job struct {
	Height uint64
	ID     [32]byte
}

// Source is anything block bodies can be pulled from: a peer, an HTTP server, static hosting.
//
// It does not have to be trustworthy. It may return fewer blocks than we ask for, in any order.
type Source interface {
	Name() string
	Fetch(ctx context.Context, ids [][32]byte) ([]*chain.Block, error)
}

// Sink is where the verified bodies are stored.
type Sink interface {
	PutBlock(b *chain.Block) error
}

var (
	ErrNoSources = errors.New("blocksync: no sources")
	// ErrBadBlock means a source sent something that does not match the manifest. It is a reason to
	// throw the source away, not to retry the request.
	ErrBadBlock = errors.New("blocksync: the body does not match the manifest")
)

// VerifyBody checks that a body belongs to the expected id **and** that the transactions match the
// merkle root in the header.
func VerifyBody(b *chain.Block, want [32]byte) error {
	if b == nil || b.Header == nil {
		return fmt.Errorf("%w: an empty block", ErrBadBlock)
	}
	if got := b.Header.ID(); got != want {
		return fmt.Errorf("%w: id %x, expected %x", ErrBadBlock, got[:6], want[:6])
	}
	if got := chain.TxMerkleRoot(b.Txs); got != b.Header.MerkleRoot {
		return fmt.Errorf("%w: the merkle root is %x, the header says %x",
			ErrBadBlock, got[:6], b.Header.MerkleRoot[:6])
	}
	return nil
}

// Stats is what can be said about a run to the operator.
type Stats struct {
	Blocks  int64
	Bytes   int64
	Dropped []string // the sources thrown away for lying or for repeated failures
}

// Downloader splits the work between the sources and measures which one keeps up.
type Downloader struct {
	// BatchSize is how many blocks are asked for at once. Smaller batches = better spread between the
	// sources, larger ones = less overhead.
	BatchSize int
	// MaxFailures is how many failures a source survives before it is thrown away. A lie throws it away at once.
	MaxFailures int
	// Timeout per batch.
	Timeout time.Duration
}

func New() *Downloader {
	return &Downloader{BatchSize: 16, MaxFailures: 3, Timeout: 30 * time.Second}
}

type worker struct {
	src      Source
	failures int
	bytes    int64
	blocks   int64
	dropped  bool
}

// Run downloads every job. It runs until they are all done, or until no source is left.
//
// The download order is not the apply order — bodies are stored as they arrive. Applying is serial and
// happens elsewhere, from disk.
func (d *Downloader) Run(ctx context.Context, jobs []Job, sources []Source, sink Sink) (Stats, error) {
	var st Stats
	if len(sources) == 0 {
		return st, ErrNoSources
	}
	if len(jobs) == 0 {
		return st, nil
	}

	queue := make(chan Job, len(jobs))
	for _, j := range jobs {
		queue <- j
	}
	// remaining is the number of blocks not yet verified and stored. A thread must not finish just
	// because the queue is **temporarily** empty — another thread may still return work.
	var remaining atomic.Int64
	remaining.Store(int64(len(jobs)))

	var (
		mu      sync.Mutex
		workers = make([]*worker, len(sources))
		alive   atomic.Int64
		blocks  atomic.Int64
		byteSum atomic.Int64
	)
	for i, s := range sources {
		workers[i] = &worker{src: s}
	}
	alive.Store(int64(len(sources)))

	var wg sync.WaitGroup
	for _, w := range workers {
		wg.Add(1)
		go func(w *worker) {
			defer wg.Done()
			for remaining.Load() > 0 && ctx.Err() == nil && !w.dropped {
				batch := drain(queue, d.BatchSize)
				if len(batch) == 0 {
					// The queue is empty, but work is still running elsewhere. Wait for a return.
					if alive.Load() == 0 {
						return
					}
					select {
					case <-ctx.Done():
						return
					case <-time.After(5 * time.Millisecond):
					}
					continue
				}

				done, err := d.fetchBatch(ctx, w, batch, sink, &blocks, &byteSum)
				remaining.Add(-int64(done))

				if err == nil {
					w.failures = 0
					continue
				}
				mu.Lock()
				if errors.Is(err, ErrBadBlock) {
					// A lie is not tolerated even once — the source is out of the game.
					w.dropped = true
					st.Dropped = append(st.Dropped, w.src.Name()+" (lied)")
				} else {
					w.failures++
					if w.failures >= d.MaxFailures {
						w.dropped = true
						st.Dropped = append(st.Dropped, w.src.Name()+" (kept failing)")
					}
				}
				mu.Unlock()

				// Return the undelivered jobs to the others.
				for _, j := range batch[done:] {
					queue <- j
				}
				if w.dropped {
					alive.Add(-1)
					return
				}
			}
		}(w)
	}
	wg.Wait()

	st.Blocks = blocks.Load()
	st.Bytes = byteSum.Load()
	if rem := remaining.Load(); rem > 0 {
		return st, fmt.Errorf("%w: %d blocks left, no source delivered them", ErrNoSources, rem)
	}
	return st, nil
}

// drain takes at most n items from the queue without blocking.
func drain(queue chan Job, n int) []Job {
	out := make([]Job, 0, n)
	for len(out) < n {
		select {
		case j := <-queue:
			out = append(out, j)
		default:
			return out
		}
	}
	return out
}

// fetchBatch downloads and verifies one batch. It returns how many jobs from the **start** of the batch
// are done — the rest goes back into the queue. That is why `batch` is reordered along the way so the
// finished ones are at the front.
func (d *Downloader) fetchBatch(ctx context.Context, w *worker, batch []Job, sink Sink,
	blocks, byteSum *atomic.Int64) (int, error) {
	ids := make([][32]byte, len(batch))
	for i, j := range batch {
		ids[i] = j.ID
	}

	fctx, cancel := context.WithTimeout(ctx, d.Timeout)
	defer cancel()
	got, err := w.src.Fetch(fctx, ids)
	if err != nil {
		return 0, err
	}

	want := make(map[[32]byte]bool, len(batch))
	for _, j := range batch {
		want[j.ID] = true
	}
	stored := make(map[[32]byte]bool, len(got))
	for _, b := range got {
		if b == nil || b.Header == nil {
			return d.compact(batch, stored), fmt.Errorf("%w: an empty block from %s", ErrBadBlock, w.src.Name())
		}
		id := b.Header.ID()
		if !want[id] {
			return d.compact(batch, stored), fmt.Errorf("%w: %s sent the block %x, which we did not ask for",
				ErrBadBlock, w.src.Name(), id[:6])
		}
		if err := VerifyBody(b, id); err != nil {
			return d.compact(batch, stored), err
		}
		if err := sink.PutBlock(b); err != nil {
			return d.compact(batch, stored), err
		}
		stored[id] = true
		blocks.Add(1)
		byteSum.Add(int64(len(b.Serialize())))
		w.blocks++
	}
	n := d.compact(batch, stored)
	if n < len(batch) {
		// What a source did not deliver is not a lie — it just did not make it or does not have it.
		return n, fmt.Errorf("%s did not deliver %d of %d blocks", w.src.Name(), len(batch)-n, len(batch))
	}
	return n, nil
}

// compact moves the finished jobs to the front of the batch and returns their count.
func (d *Downloader) compact(batch []Job, stored map[[32]byte]bool) int {
	n := 0
	for i := range batch {
		if stored[batch[i].ID] {
			batch[n], batch[i] = batch[i], batch[n]
			n++
		}
	}
	return n
}
