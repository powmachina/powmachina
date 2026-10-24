package main

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"powmachina/internal/chain"
)

// netStats is what the node counts about the network as it runs, for /metrics: the shares it took
// and why it refused the others, the blocks its miners submitted, the reorgs, how long a block takes
// to process and how long after the node first heard of a block it was applied.. Without these the
// Sharepool's behaviour under load is a guess.
type netStats struct {
	mu sync.Mutex

	shares  map[string]uint64 // result → count
	submits map[string]uint64 // result → count (submitblock from a local miner or the stratum port)
	attests map[string]uint64 // result → count (signatures from miners on the stratum port)

	reorgs, reorgBlocks, reorgDepthMax uint64
	siblingSwitches                    uint64

	process       hist // seconds to validate and apply one block
	seenToApplied hist // seconds from the node first hearing of a block to applying it

	seen     map[[32]byte]time.Time // when a header was first heard of
	tipSeen  time.Time              // when the applied tip was first heard of
	tipBytes int                    // the applied tip's size
	tipSet   int                    // shares in the applied tip's settlement
	tipSetB  int                    // the settlement body's bytes
	tipUnits uint64                 // share units the applied tip paid (its settlement + its own)

	creditKeys       int
	creditKeysHeight uint64
}

// hist is a Prometheus histogram with fixed buckets.
type hist struct {
	counts []uint64
	sum    float64
	n      uint64
}

var statBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30}

func (h *hist) observe(v float64) {
	if h.counts == nil {
		h.counts = make([]uint64, len(statBuckets))
	}
	for i, b := range statBuckets {
		if v <= b {
			h.counts[i]++
		}
	}
	h.sum += v
	h.n++
}

func (s *netStats) inc(m *map[string]uint64, result string) {
	s.mu.Lock()
	if *m == nil {
		*m = map[string]uint64{}
	}
	(*m)[result]++
	s.mu.Unlock()
}

// heard records the first time a header was heard of (from a peer, or a local miner).
func (s *netStats) heard(id [32]byte, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seen == nil {
		s.seen = map[[32]byte]time.Time{}
	}
	if _, ok := s.seen[id]; ok {
		return
	}
	// A day of blocks at most; a sync from zero hears thousands at once and needs none of them.
	if len(s.seen) >= 4096 {
		clear(s.seen)
	}
	s.seen[id] = now
}

// applied records a block the node applied.
func (s *netStats) applied(b *chain.Block, took time.Duration, now time.Time) {
	id := b.Header.ID()
	units := uint64(1) << b.Share.Weight
	for _, sh := range b.Settlement.Shares {
		if int(sh.Job) < len(b.Settlement.Jobs) {
			units += uint64(1) << b.Settlement.Jobs[sh.Job].Weight
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.process.observe(took.Seconds())
	if t, ok := s.seen[id]; ok {
		s.seenToApplied.observe(now.Sub(t).Seconds())
		s.tipSeen = t
		delete(s.seen, id)
	} else {
		s.tipSeen = now
	}
	s.tipBytes = len(b.Serialize())
	s.tipSet = len(b.Settlement.Shares)
	s.tipSetB = len(b.Settlement.Serialize())
	s.tipUnits = units
}

// reorged records a reorg that rolled back `depth` blocks.
func (s *netStats) reorged(depth uint64) {
	s.mu.Lock()
	s.reorgs++
	s.reorgBlocks += depth
	s.reorgDepthMax = max(s.reorgDepthMax, depth)
	s.mu.Unlock()
}

func (s *netStats) siblingSwitched() {
	s.mu.Lock()
	s.siblingSwitches++
	s.mu.Unlock()
}

// shareResult names why a share was or was not taken, for the counter's label.
func shareResult(err error) string {
	switch {
	case err == nil:
		return "accepted"
	case errors.Is(err, errLightNode):
		return "light_node"
	case errors.Is(err, errBadShare):
		return "bad"
	case errors.Is(err, chain.ErrUnknownParent):
		return "unknown_parent" // it waits for its block (shareorphans.go)
	case errors.Is(err, chain.ErrShareExpired):
		return "expired"
	default:
		return "refused"
	}
}

// write adds the metrics to a scrape.
func (s *netStats) write(b *strings.Builder, n *Node) {
	s.mu.Lock()
	defer s.mu.Unlock()
	labelled := func(name, help string, m map[string]uint64, results ...string) {
		fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s counter\n", name, help, name)
		// The usual results are always there, so a rate() over them starts from zero, not from a gap.
		all := map[string]uint64{}
		for _, r := range results {
			all[r] = 0
		}
		for k, v := range m {
			all[k] = v
		}
		keys := make([]string, 0, len(all))
		for k := range all {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(b, "%s{%s} %d\n", name, label("result", k), all[k])
		}
	}
	labelled("powm_shares_received_total", "Sharepool shares received (P2P, stratum, RPC) by result", s.shares,
		"accepted", "known", "bad", "unknown_parent", "expired", "refused")
	labelled("powm_blocks_submitted_total", "blocks submitted by local miners (stratum, RPC) by result", s.submits,
		"accepted", "stale", "duplicate", "invalid")
	labelled("powm_attestations_stratum_total", "finality signatures from miners on the stratum port by result", s.attests,
		"accepted", "refused")
	counter := func(name, help string, v uint64) {
		fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s counter\n%s %d\n", name, help, name, name, v)
	}
	gauge := func(name, help string, v float64) {
		fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s gauge\n%s %s\n", name, help, name, name, strconv.FormatFloat(v, 'f', -1, 64))
	}
	gauge("powm_orphan_shares", "shares waiting for the block they build on", float64(n.shareOrphans.len()))
	counter("powm_reorgs_total", "reorgs of the UTXO set", s.reorgs)
	counter("powm_reorg_blocks_total", "blocks rolled back by reorgs", s.reorgBlocks)
	gauge("powm_reorg_depth_max", "the deepest reorg since the node started", float64(s.reorgDepthMax))
	counter("powm_sibling_switches_total", "times share votes made a sibling of the tip the preferred block", s.siblingSwitches)
	histogram(b, "powm_block_process_seconds", "seconds to validate and apply one block", &s.process)
	histogram(b, "powm_block_seen_to_applied_seconds", "seconds from first hearing of a block to applying it", &s.seenToApplied)
	if !s.tipSeen.IsZero() {
		// Compared across nodes it is how long a block took to reach each of them.
		gauge("powm_tip_seen_timestamp_seconds", "unix time this node first heard of the applied tip",
			float64(s.tipSeen.UnixMilli())/1000)
		gauge("powm_tip_block_bytes", "size of the applied tip", float64(s.tipBytes))
		gauge("powm_tip_settled_shares", "shares the applied tip settled", float64(s.tipSet))
		gauge("powm_tip_settlement_bytes", "size of the applied tip's settlement body", float64(s.tipSetB))
		gauge("powm_tip_settled_units", "share units the applied tip paid, its own included", float64(s.tipUnits))
	}
	// The number of keys with a credit, counted once per block: it is what the credit step's cost
	// grows with.
	if tip, err := n.store.Tip(); err == nil {
		if tip.Height != s.creditKeysHeight || s.creditKeys == 0 {
			count := 0
			_ = n.store.ForEachCredit(func([32]byte, chain.Credit) error { count++; return nil })
			s.creditKeys, s.creditKeysHeight = count, tip.Height
		}
		gauge("powm_credit_keys", "keys holding a credit", float64(s.creditKeys))
		gauge("powm_credit_pot_cogs", "the Sharepool's pot", float64(n.store.Pot()))
	}
}

func histogram(b *strings.Builder, name, help string, h *hist) {
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s histogram\n", name, help, name)
	for i, le := range statBuckets {
		c := uint64(0)
		if h.counts != nil {
			c = h.counts[i]
		}
		fmt.Fprintf(b, "%s_bucket{le=\"%s\"} %d\n", name, strconv.FormatFloat(le, 'f', -1, 64), c)
	}
	fmt.Fprintf(b, "%s_bucket{le=\"+Inf\"} %d\n%s_sum %s\n%s_count %d\n", name, h.n, name,
		strconv.FormatFloat(h.sum, 'f', -1, 64), name, h.n)
}
