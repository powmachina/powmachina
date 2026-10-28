package explorer

import (
	"context"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"
)

// The Rigs page: the devices mining on the chain's public stratum right now, as the nodes report
// them (getstratuminfo). Lane C is RandomX on the processor, lane G runs on the graphics card. The
// hashrate is the node's own estimate from the shares the device sends, and the agent is the line
// the miner announces at login — both are for display only; nothing a reward depends on is in here.
//
// The device's address on the network (its IP) is NOT carried out: the node knows it — it is in the
// node's getstratuminfo — but Rig has no field for it, so it is dropped when the answer is decoded
// and never reaches the page.

// Rig is one worker on the public stratum, as the page shows it: a payout address and a worker
// name taken together. A miner that opens several connections under the same name — one per thread,
// or the same rig on both public nodes — is one row here, its connections summed; Conns says how
// many there are.
type Rig struct {
	Address   string   `json:"address"`
	Worker    string   `json:"worker"`
	Agent     string   `json:"agent"`
	Since     int64    `json:"since"`
	Lanes     []string `json:"lanes"`
	Accepted  uint64   `json:"accepted"`
	Rejected  uint64   `json:"rejected"`
	Blocks    uint64   `json:"blocks"`
	HashrateC float64  `json:"hashrate_c"`
	HashrateG float64  `json:"hashrate_g"`
	Conns     int      `json:"conns"`
}

// stratumInfo is one node's getstratuminfo answer. A worker's `remote` field has no home in Rig, so
// it is ignored here and the miner's IP stays on the node.
type stratumInfo struct {
	Workers []Rig `json:"workers"`
}

// rigsData is the Rigs page.
type rigsData struct {
	Rigs   []Rig
	LaneC  int     // devices mining lane C
	LaneG  int     // devices mining lane G
	TotalC float64 // summed lane-C hashrate estimate, H/s
	TotalG float64
}

// formatHashrateFloat formats an estimate in H/s, reusing the string formatter so the units match
// the rest of the explorer. A device that has not been measured yet (0) shows a dash.
func formatHashrateFloat(h float64) string {
	if h <= 0 {
		return "—"
	}
	return FormatHashrate(strconv.FormatFloat(h, 'f', 0, 64))
}

// gatherRigs asks every node the explorer knows (the main node and the public ones it sums the map
// from) for its stratum connections and folds them into one row per worker. A node without stratum,
// or one that is down, answers with an error and is simply left out — the page shows whoever did
// answer. Each live connection lives on exactly one node, so nothing is double-counted; the several
// connections of one worker (its threads, or the same rig on both public nodes) are summed, and the
// representative connection — the one that has accepted the most shares — lends its agent string.
func (s *Server) gatherRigs(ctx context.Context) []Rig {
	nodes := append([]*Client{s.cli}, s.cfg.GeoNodes...)
	lists := make([][]Rig, len(nodes))
	var wg sync.WaitGroup
	for i, n := range nodes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			nctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()
			var si stratumInfo
			if err := n.Call(nctx, "getstratuminfo", nil, &si); err != nil {
				return
			}
			lists[i] = si.Workers
		}()
	}
	wg.Wait()

	type agg struct {
		rig      Rig
		lanes    map[string]bool
		topAccpt uint64 // the accepted count of the connection whose agent is held
	}
	by := map[string]*agg{}
	var order []string
	for _, ws := range lists {
		for _, c := range ws {
			key := c.Address + "\x00" + c.Worker
			a := by[key]
			if a == nil {
				a = &agg{rig: Rig{Address: c.Address, Worker: c.Worker, Agent: c.Agent, Since: c.Since},
					lanes: map[string]bool{}, topAccpt: c.Accepted}
				by[key] = a
				order = append(order, key)
			}
			a.rig.Conns++
			a.rig.Accepted += c.Accepted
			a.rig.Rejected += c.Rejected
			a.rig.Blocks += c.Blocks
			a.rig.HashrateC += c.HashrateC
			a.rig.HashrateG += c.HashrateG
			if c.Since > 0 && (a.rig.Since == 0 || c.Since < a.rig.Since) {
				a.rig.Since = c.Since // the earliest connection — the worker's uptime
			}
			if c.Accepted >= a.topAccpt && c.Agent != "" {
				a.rig.Agent, a.topAccpt = c.Agent, c.Accepted
			}
			for _, l := range c.Lanes {
				a.lanes[l] = true
			}
		}
	}

	out := make([]Rig, 0, len(order))
	for _, key := range order {
		a := by[key]
		for _, l := range []string{"C", "G"} {
			if a.lanes[l] {
				a.rig.Lanes = append(a.rig.Lanes, l)
			}
		}
		out = append(out, a.rig)
	}
	sort.Slice(out, func(i, j int) bool {
		hi, hj := out[i].HashrateC+out[i].HashrateG, out[j].HashrateC+out[j].HashrateG
		if hi != hj {
			return hi > hj
		}
		if out[i].Accepted != out[j].Accepted {
			return out[i].Accepted > out[j].Accepted
		}
		return out[i].Address < out[j].Address
	})
	return out
}

// handleRigs shows the devices mining on the public stratum right now.
func (s *Server) handleRigs(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.Timeout)
	defer cancel()
	rigs := s.gatherRigs(ctx)
	d := rigsData{Rigs: rigs}
	for _, rg := range rigs {
		d.TotalC += rg.HashrateC
		d.TotalG += rg.HashrateG
		for _, l := range rg.Lanes {
			switch l {
			case "C":
				d.LaneC++
			case "G":
				d.LaneG++
			}
		}
	}
	s.render(w, r, "rigs.html", "Rigs", "rigs", d)
}
