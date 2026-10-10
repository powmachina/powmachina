package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"time"

	"powmachina/internal/blocksync"
	"powmachina/internal/chain"
	"powmachina/internal/p2p"
)

// Bulk downloading of bodies.
//
// The ordinary P2P sync pulls bodies one at a time from one peer and is therefore slow even on
// a fast link. Here `internal/blocksync` is used: the header chain is a **verified manifest**,
// so bodies can be pulled from any number of sources at once and each one is checked against a
// known hash.
//
// The download order is not the apply order. Bodies land on disk as they arrive; applying is
// serial and **validating** — bodies above the tip are unverified until their turn comes.

// peerSource is a blocksync.Source over a single P2P connection.
//
// It works because the bulk sync runs on a dedicated connection in a blocking loop: send
// getdata, receive the answers. On a connection that also serves ordinary traffic the answers
// would have to be correlated — for a bulk sync that is not needed.
type peerSource struct {
	conn *p2p.Conn
	name string
}

func (p *peerSource) Name() string { return p.name }

func (p *peerSource) Fetch(ctx context.Context, ids [][32]byte) ([]*chain.Block, error) {
	if len(ids) > p2p.MaxGetDataItems {
		ids = ids[:p2p.MaxGetDataItems]
	}
	payload, err := p2p.EncodeGetData(p2p.BlockItems(ids))
	if err != nil {
		return nil, err
	}
	if err := p.conn.Send(&p2p.Message{Command: p2p.CmdGetData, Payload: payload}); err != nil {
		return nil, err
	}
	out := make([]*chain.Block, 0, len(ids))
	for range ids {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		msg, err := p.conn.Recv()
		if err != nil {
			return out, err
		}
		if msg.Command != p2p.CmdBlock {
			continue
		}
		blk, err := p2p.DecodeBlock(msg.Payload)
		if err != nil {
			return out, err
		}
		out = append(out, blk)
	}
	return out, nil
}

// httpSources are the HTTP mirrors of the chain (`-blocksource`). They are **accelerators, not
// authorities**: every body is verified against the manifest, so a server that lies achieves nothing.
func (n *Node) httpSources() []blocksync.Source {
	out := make([]blocksync.Source, 0, len(n.blockSources))
	for _, u := range n.blockSources {
		out = append(out, blocksync.NewHTTPSource(u))
	}
	return out
}

// bulkFetch pulls the missing bodies up to height `upTo` and applies them serially.
//
// `conn` may be nil — then bodies are pulled from the HTTP mirrors only. When there is not a
// single source, it returns an error; the caller should then fall back to the classic
// one-at-a-time path.
func (n *Node) bulkFetch(ctx context.Context, conn *p2p.Conn, peerName string, upTo uint64) error {
	jobs, err := n.missingBodyJobs(upTo)
	if err != nil || len(jobs) == 0 {
		return err
	}

	sources := n.httpSources()
	if conn != nil {
		sources = append(sources, &peerSource{conn: conn, name: peerName})
	}
	if len(sources) == 0 {
		return blocksync.ErrNoSources
	}

	d := blocksync.New()
	start := time.Now()
	st, err := d.Run(ctx, jobs, sources, n.store)
	if err != nil {
		return err
	}
	dur := time.Since(start)
	mbs := float64(st.Bytes) / 1e6 / max(dur.Seconds(), 0.001)
	log.Printf("downloaded %d bodies from %d sources in %s (%.1f MB/s)",
		st.Blocks, len(sources), dur.Round(time.Millisecond), mbs)
	for _, dr := range st.Dropped {
		log.Printf("⚠ source dropped: %s", dr)
	}

	// Applying is serial and validating — the bodies above the tip have been unverified so far.
	// It goes through the same path as ordinary operation (the branch index, the UTXO reorg,
	// invalid branches), so that there is a single owner of applying.
	return n.ApplyReadyBodies()
}

// missingBodyJobs assembles the manifest: the heights whose header we have verified but whose body is missing.
func (n *Node) missingBodyJobs(upTo uint64) ([]blocksync.Job, error) {
	tip, err := n.store.Tip()
	if err != nil {
		return nil, err
	}
	from := tip.Height + 1
	if from > upTo {
		return nil, nil
	}
	var jobs []blocksync.Job
	for h := from; h <= upTo; h++ {
		id, ok := n.chain.BlockIDAt(h)
		if !ok {
			break // we have no headers beyond this, the manifest ends here
		}
		if n.store.HasBody(id) {
			continue
		}
		jobs = append(jobs, blocksync.Job{Height: h, ID: id})
	}
	if len(jobs) > 0 {
		log.Printf("%d block bodies missing (heights %d–%d), pulling in parallel",
			len(jobs), jobs[0].Height, jobs[len(jobs)-1].Height)
	}
	return jobs, nil
}

// ServeBlocksHTTP turns on the HTTP mirror of block bodies. It is **opt-in**: an open endpoint
// that pours out gigabytes on request is a DoS amplifier, so it has both a concurrency cap and
// a budget.
//
// Blocks are immutable, so the answers are cacheable and an ordinary CDN can be put in front of this.
func (n *Node) ServeBlocksHTTP(addr string, byteBudget int64) error {
	h := blocksync.LimitedHandler(n.store, 16, byteBudget)
	log.Printf("the HTTP block mirror is listening on %s%s (budget %s)",
		addr, blocksync.BlockPath, humanBytes(byteBudget))
	srv := &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 10 * time.Second}
	return srv.ListenAndServe()
}

func humanBytes(n int64) string {
	switch {
	case n <= 0:
		return "no limit"
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	default:
		return fmt.Sprintf("%.0f MiB", float64(n)/(1<<20))
	}
}
