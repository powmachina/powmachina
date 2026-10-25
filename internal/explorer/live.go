package explorer

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"sort"
	"sync"
	"time"
)

// The live view of the Sharepool (GET <base>/api/sharepool): what the share pool holds right now,
// the last blocks with who closed them and whom they paid, and the miners of the last half hour —
// a leaderboard with each one's credit toward its payout. It feeds the animated page on the website,
// which polls it every few seconds — so one answer is shared by every visitor for liveTTL, and a
// block's payees are asked for once per block, not once per visit.
//
// GET <base>/api/sharepool/miners is every miner of the same window with its icon, for the page to
// draw them all; it changes slowly and is the heavy one, so it is cached for longer.

const (
	// liveTTL: a lane block is 60 s, shares arrive about twice a second; two seconds of delay is
	// invisible in an animation and keeps the node out of the visitors' reach.
	liveTTL = 2 * time.Second
	// liveBlocks is how many of the last blocks the answer carries, liveMiners how many miners of
	// the pool and of each block — the rest is summed, as the mosaic does.
	liveBlocks = 16
	liveMiners = 40
	// liveWindow is how many blocks the leaderboard and the rates look back over: half an hour at
	// a 30 s block. liveTop is the length of the leaderboard.
	liveWindow = 60
	liveTop    = 10
	// liveMinersTTL and liveMinersMax: the field of every miner.
	liveMinersTTL = 15 * time.Second
	liveMinersMax = 5000
	// livePaidMax bounds the cache of blocks' settlements; it is cleared when it grows past it, and
	// the next build asks the window's blocks again.
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

// LiveWindow is the last liveWindow blocks and the pool taken together.
type LiveWindow struct {
	Blocks  int    `json:"blocks"`
	Seconds int64  `json:"seconds"`
	Shares  int    `json:"shares"`
	Units   uint64 `json:"units"`
	Miners  int    `json:"miners"`
	// PaidOut is how many miners' last payout falls into the window.
	PaidOut int `json:"paid_out"`
}

// LiveTop is one row of the leaderboard: the miner's work in the window, and its credit.
type LiveTop struct {
	LiveMiner
	// Permille is the miner's part of the window's units, in whole per mille.
	Permille   uint64 `json:"permille"`
	CreditCogs int64  `json:"credit_cogs"`
	NeedCogs   int64  `json:"need_cogs"`
	// PaidAt is the height of the miner's last payout, 0 = never paid.
	PaidAt uint64 `json:"paid_at"`
}

// LiveGeo is where the shares the node took first came from, by country (getsharegeo): the
// country of the address each came in from. Off when the node has no GeoIP database.
type LiveGeo struct {
	Enabled   bool          `json:"enabled"`
	Minutes   int           `json:"minutes"`
	Unknown   int           `json:"unknown"`
	Countries []LiveCountry `json:"countries"`
}

// LiveCountry is one country of the map: its shares in the window and since the node started.
type LiveCountry struct {
	Country string `json:"country"`
	Shares  int    `json:"shares"`
	Seen    uint64 `json:"seen"`
}

// mergeGeo sums two nodes' maps. Each node counts only the shares that came in through it
// (-geoip-ignore leaves out what the network's own nodes relay), so the sum counts a share once.
func mergeGeo(a, b LiveGeo) LiveGeo {
	if !b.Enabled {
		return a
	}
	if !a.Enabled {
		a.Enabled, a.Minutes = true, b.Minutes
	}
	a.Unknown += b.Unknown
	at := map[string]int{}
	for i, c := range a.Countries {
		at[c.Country] = i
	}
	for _, c := range b.Countries {
		if i, ok := at[c.Country]; ok {
			a.Countries[i].Shares += c.Shares
			a.Countries[i].Seen += c.Seen
			continue
		}
		at[c.Country] = len(a.Countries)
		a.Countries = append(a.Countries, c)
	}
	sort.SliceStable(a.Countries, func(i, j int) bool {
		if a.Countries[i].Shares != a.Countries[j].Shares {
			return a.Countries[i].Shares > a.Countries[j].Shares
		}
		return a.Countries[i].Country < a.Countries[j].Country
	})
	return a
}

// Live is the whole answer.
type Live struct {
	Network     string      `json:"network"`
	Height      uint64      `json:"height"`
	TipTime     int64       `json:"tip_time"`
	TakesShares bool        `json:"takes_shares"`
	Pool        LiveTally   `json:"pool"`
	Blocks      []LiveBlock `json:"blocks"`
	Window      LiveWindow  `json:"window"`
	Top         []LiveTop   `json:"top"`
	Geo         LiveGeo     `json:"geo"`
	Now         int64       `json:"now"`
}

// LiveMiners is the answer of /api/sharepool/miners: every miner of the window, the most units
// first.
type LiveMiners struct {
	Height uint64      `json:"height"`
	Count  int         `json:"count"`
	Miners []LiveMiner `json:"miners"`
}

// Credits is getcredits.
type Credits struct {
	Height  uint64 `json:"height"`
	PotCogs int64  `json:"pot_cogs"`
	Count   int    `json:"count"`
	Credits []struct {
		Address    string `json:"address"`
		AmountCogs int64  `json:"amount_cogs"`
		NeedCogs   int64  `json:"need_cogs"`
		LastShare  uint64 `json:"last_share"`
		PaidAt     uint64 `json:"paid_at"`
		ReadyAt    uint64 `json:"ready_at"`
	} `json:"credits"`
}

type liveCache struct {
	mu   sync.Mutex
	at   time.Time
	body []byte
	// paid is each block's settlement as the node told it, by block id.
	paid map[string]ShareTally
	// window is the miners of the last build, for the miners answer.
	window       []MinerShare
	windowHeight uint64
	minersAt     time.Time
	minersBody   []byte
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

// handleLiveMiners answers GET <base>/api/sharepool/miners from the window of the last live build.
func (s *Server) handleLiveMiners(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	if _, err := s.live.get(func() ([]byte, error) { return s.buildLive(r.Context()) }); err != nil {
		log.Printf("api/sharepool/miners: %v", err)
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":"the node is not responding"}`))
		return
	}
	c := &s.live
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.minersBody == nil || time.Since(c.minersAt) >= liveMinersTTL {
		out := LiveMiners{Height: c.windowHeight, Count: len(c.window), Miners: []LiveMiner{}}
		for i, m := range c.window {
			if i == liveMinersMax {
				break
			}
			out.Miners = append(out.Miners, LiveMiner{Address: m.Address, Shares: m.Shares, Units: m.Units, Icon: liveIcon(m.Address)})
		}
		body, err := json.Marshal(out)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		c.minersBody, c.minersAt = body, time.Now()
	}
	_, _ = w.Write(c.minersBody)
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
	if err := s.cli.Call(ctx, "getblocks", map[string]any{"count": liveWindow}, &list); err != nil {
		return nil, err
	}
	// The credits are for the leaderboard only: a node that does not have getcredits yet still
	// gives the rest of the answer.
	var credits Credits
	if err := s.cli.Call(ctx, "getcredits", nil, &credits); err != nil {
		log.Printf("api/sharepool: getcredits: %v", err)
	}

	// The map, the same way: an older node without getsharegeo leaves it off.
	var geo LiveGeo
	if err := s.cli.Call(ctx, "getsharegeo", nil, &geo); err != nil {
		geo = LiveGeo{}
	}
	// The other nodes at once and briefly: a public node that is down must not hold up the page.
	more := make([]LiveGeo, len(s.cfg.GeoNodes))
	var wg sync.WaitGroup
	for i, n := range s.cfg.GeoNodes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			gctx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
			defer cancel()
			if err := n.Call(gctx, "getsharegeo", nil, &more[i]); err != nil {
				more[i] = LiveGeo{}
			}
		}()
	}
	wg.Wait()
	for _, m := range more {
		geo = mergeGeo(geo, m)
	}

	c := &s.live
	if c.paid == nil || len(c.paid) > livePaidMax {
		c.paid = map[string]ShareTally{}
	}
	out := Live{Network: info.Network, Height: info.Height, TipTime: info.TipTime, TakesShares: pool.TakesShares,
		Pool: liveTally(pool.ShareTally), Blocks: []LiveBlock{}, Top: []LiveTop{}, Geo: geo, Now: time.Now().Unix()}
	if out.Geo.Countries == nil {
		out.Geo.Countries = []LiveCountry{}
	}

	// The window: the pool and the settlements of the last blocks, summed by miner.
	sum := map[string]*MinerShare{}
	add := func(t ShareTally) {
		out.Window.Shares += t.Shares
		out.Window.Units += t.Units
		for _, m := range t.Miners {
			e := sum[m.Address]
			if e == nil {
				e = &MinerShare{Address: m.Address}
				sum[m.Address] = e
			}
			e.Shares += m.Shares
			e.Units += m.Units
		}
	}
	add(pool.ShareTally)
	oldest := out.Now
	var oldestHeight uint64
	for i, b := range list.Blocks {
		var paid ShareTally
		if t, ok := c.paid[b.ID]; ok {
			paid = t
		} else if b.Shares > 0 {
			var blk Block
			if err := s.cli.Call(ctx, "getblock", map[string]any{"id": b.ID}, &blk); err == nil && blk.Settlement != nil {
				paid = *blk.Settlement
				c.paid[b.ID] = paid
			}
		} else if !b.Pending() {
			c.paid[b.ID] = paid
		}
		add(paid)
		out.Window.Blocks++
		oldest, oldestHeight = b.Time, b.Height
		if i < liveBlocks {
			lb := LiveBlock{Height: b.Height, ID: b.ID, Lane: b.Lane, Algo: AlgoName(b.Lane), Time: b.Time, Paid: liveTally(paid)}
			if b.Payout != "" {
				lb.Closer = &LiveMiner{Address: b.Payout, Icon: liveIcon(b.Payout)}
			}
			out.Blocks = append(out.Blocks, lb)
		}
	}
	if out.Now > oldest {
		out.Window.Seconds = out.Now - oldest
	}
	window := make([]MinerShare, 0, len(sum))
	for _, m := range sum {
		window = append(window, *m)
	}
	sort.Slice(window, func(i, j int) bool {
		if window[i].Units != window[j].Units {
			return window[i].Units > window[j].Units
		}
		return window[i].Address < window[j].Address
	})
	out.Window.Miners = len(window)
	c.window, c.windowHeight = window, info.Height

	byAddr := map[string]int{}
	for i, cr := range credits.Credits {
		byAddr[cr.Address] = i
		if cr.PaidAt != 0 && oldestHeight != 0 && cr.PaidAt >= oldestHeight {
			out.Window.PaidOut++
		}
	}
	for i, m := range window {
		if i == liveTop {
			break
		}
		row := LiveTop{LiveMiner: LiveMiner{Address: m.Address, Shares: m.Shares, Units: m.Units, Icon: liveIcon(m.Address)}}
		if out.Window.Units > 0 {
			row.Permille = m.Units * 1000 / out.Window.Units
		}
		if j, ok := byAddr[m.Address]; ok {
			cr := credits.Credits[j]
			row.CreditCogs, row.NeedCogs, row.PaidAt = cr.AmountCogs, cr.NeedCogs, cr.PaidAt
		}
		out.Top = append(out.Top, row)
	}
	return json.Marshal(out)
}
