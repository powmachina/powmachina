package explorer

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"
)

// The live view of the Sharepool (GET <base>/api/sharepool): what the share pool holds right now,
// and the last blocks with who closed them and whom they paid. It feeds the animated page on the
// website, which polls it every few seconds — so one answer is shared by every visitor for
// liveTTL, and a block's payees are asked for once per block, not once per visit.

const (
	// liveTTL: a lane block is 60 s, shares arrive about twice a second; two seconds of delay is
	// invisible in an animation and keeps the node out of the visitors' reach.
	liveTTL = 2 * time.Second
	// liveBlocks is how many of the last blocks the answer carries, liveMiners how many miners of
	// the pool and of each block — the rest is summed, as the mosaic does.
	liveBlocks = 12
	liveMiners = 40
	// livePaidMax bounds the cache of blocks' payees; a block older than the window is never
	// asked for again, so the cache is simply cleared when it grows past it.
	livePaidMax = 256
)

// LiveIcon is an identicon as the page draws it: the 25 cells row by row as bits, and the colour.
type LiveIcon struct {
	Cells uint32 `json:"cells"`
	Color string `json:"color"`
}

// LiveMiner is one miner's part of the pool or of a block.
type LiveMiner struct {
	Address string   `json:"address"`
	Shares  int      `json:"shares"`
	Units   uint64   `json:"units"`
	Icon    LiveIcon `json:"icon"`
}

// LiveTally is a ShareTally with the icons drawn.
type LiveTally struct {
	Shares     int         `json:"shares"`
	Units      uint64      `json:"units"`
	MinerCount int         `json:"miner_count"`
	Miners     []LiveMiner `json:"miners"`
}

// LiveBlock is one of the last blocks.
type LiveBlock struct {
	Height uint64     `json:"height"`
	ID     string     `json:"id"`
	Lane   string     `json:"lane"`
	Algo   string     `json:"algo"`
	Time   int64      `json:"time"`
	Closer *LiveMiner `json:"closer"`
	// Paid is who the shares the block settles belong to (not the block's own share).
	Paid LiveTally `json:"paid"`
}

// Live is the whole answer.
type Live struct {
	Network     string      `json:"network"`
	Height      uint64      `json:"height"`
	TipTime     int64       `json:"tip_time"`
	TakesShares bool        `json:"takes_shares"`
	Pool        LiveTally   `json:"pool"`
	Blocks      []LiveBlock `json:"blocks"`
	Now         int64       `json:"now"`
}

type liveCache struct {
	mu   sync.Mutex
	at   time.Time
	body []byte
	paid map[string]LiveTally
}

func liveIcon(addr string) LiveIcon {
	ic := iconOf(addr)
	var cells uint32
	for row := range ic.Cells {
		for col, on := range ic.Cells[row] {
			if on {
				cells |= 1 << (row*len(ic.Cells[row]) + col)
			}
		}
	}
	return LiveIcon{Cells: cells, Color: ic.Color()}
}

func liveTally(t ShareTally) LiveTally {
	out := LiveTally{Shares: t.Shares, Units: t.Units, MinerCount: t.MinerCount, Miners: []LiveMiner{}}
	for i, m := range t.Miners {
		if i == liveMiners {
			break
		}
		out.Miners = append(out.Miners, LiveMiner{Address: m.Address, Shares: m.Shares, Units: m.Units, Icon: liveIcon(m.Address)})
	}
	return out
}

// handleLive answers GET <base>/api/sharepool.
func (s *Server) handleLive(w http.ResponseWriter, r *http.Request) {
	body, err := s.live.get(func() ([]byte, error) { return s.buildLive(r.Context()) })
	w.Header().Set("Cache-Control", "no-store")
	if err != nil {
		log.Printf("api/sharepool: %v", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":"the node is not responding"}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

// get returns the cached answer while it is fresh, building a new one otherwise. Concurrent
// visitors wait for the one build instead of each asking the node.
func (c *liveCache) get(build func() ([]byte, error)) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.body != nil && time.Since(c.at) < liveTTL {
		return c.body, nil
	}
	body, err := build()
	if err != nil {
		return nil, err
	}
	c.body, c.at = body, time.Now()
	return body, nil
}

// buildLive is called under the cache's lock, so it may use the payee cache freely.
func (s *Server) buildLive(parent context.Context) ([]byte, error) {
	ctx, cancel := context.WithTimeout(parent, s.cfg.Timeout)
	defer cancel()

	var info Info
	if err := s.cli.Call(ctx, "getinfo", nil, &info); err != nil {
		return nil, err
	}
	var pool SharePool
	if err := s.cli.Call(ctx, "getsharepool", nil, &pool); err != nil {
		return nil, err
	}
	var list BlockList
	if err := s.cli.Call(ctx, "getblocks", map[string]any{"count": liveBlocks}, &list); err != nil {
		return nil, err
	}

	c := &s.live
	if c.paid == nil || len(c.paid) > livePaidMax {
		c.paid = map[string]LiveTally{}
	}
	out := Live{Network: info.Network, Height: info.Height, TipTime: info.TipTime, TakesShares: pool.TakesShares,
		Pool: liveTally(pool.ShareTally), Blocks: []LiveBlock{}, Now: time.Now().Unix()}
	for _, b := range list.Blocks {
		lb := LiveBlock{Height: b.Height, ID: b.ID, Lane: b.Lane, Algo: AlgoName(b.Lane), Time: b.Time,
			Paid: LiveTally{Miners: []LiveMiner{}}}
		if b.Payout != "" {
			lb.Closer = &LiveMiner{Address: b.Payout, Icon: liveIcon(b.Payout)}
		}
		if paid, ok := c.paid[b.ID]; ok {
			lb.Paid = paid
		} else if b.Shares > 0 {
			var blk Block
			if err := s.cli.Call(ctx, "getblock", map[string]any{"id": b.ID}, &blk); err == nil && blk.Settlement != nil {
				lb.Paid = liveTally(*blk.Settlement)
				c.paid[b.ID] = lb.Paid
			}
		} else if !b.Pending() {
			c.paid[b.ID] = lb.Paid
		}
		out.Blocks = append(out.Blocks, lb)
	}
	return json.Marshal(out)
}
