package main

import (
	"encoding/hex"
	"fmt"
	"math"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"time"

	"powmachina/internal/chain"
	"powmachina/internal/mh"
)

// Metrics in the Prometheus text format.
//
// Written by hand rather than through a client library: the binary has five direct
// dependencies today (`AUDIT.md` §1), and adding a sixth for a format that takes ten lines
// would be a bad trade.
//
// The most important metric here is **powm_tip_age_seconds**. A chain can stop for reasons no
// node reports as an error (the rotation rule with one lane missing, a dead miner), and without
// it somebody finds out by accident. An alert on this metric is what turns that into operations
// rather than luck.

// ServeMetricsHTTP turns on the metrics endpoint. It is **read only** and has no token:
// it belongs on an internal address, not on the internet.
func (n *Node) ServeMetricsHTTP(addr string) error {
	srv := &http.Server{Addr: addr, Handler: n.metricsHandler(), ReadHeaderTimeout: 10 * time.Second}
	return srv.ListenAndServe()
}

// metricsHandler is separate from ServeMetricsHTTP so the test goes through the same handler
// as the node, and not through a copy of it.
func (n *Node) metricsHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = w.Write([]byte(n.metrics(time.Now())))
	})
	return mux
}

func (n *Node) metrics(now time.Time) string {
	var b strings.Builder
	// A metric name may have a `# HELP`/`# TYPE` header **only once** per scrape — a second
	// header is a parse error for Prometheus and throws away the whole scrape, not one line.
	// So we remember the names; metrics with a label (lane) would otherwise be printed N times.
	seen := make(map[string]bool)
	g := func(name, help string, v float64, labels ...string) {
		if !seen[name] {
			seen[name] = true
			fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s gauge\n", name, help, name)
		}
		if len(labels) == 0 {
			fmt.Fprintf(&b, "%s %s\n", name, strconv.FormatFloat(v, 'f', -1, 64))
			return
		}
		fmt.Fprintf(&b, "%s{%s} %s\n", name, strings.Join(labels, ","),
			strconv.FormatFloat(v, 'f', -1, 64))
	}
	// A counter only ever grows; Prometheus computes rates from it itself. The distinction
	// from a gauge matters: `rate()` over a gauge is nonsense.
	c := func(name, help string, v float64) {
		if !seen[name] {
			seen[name] = true
			fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s counter\n", name, help, name)
		}
		fmt.Fprintf(&b, "%s %s\n", name, strconv.FormatFloat(v, 'f', -1, 64))
	}
	bl := func(x bool) float64 {
		if x {
			return 1
		}
		return 0
	}

	// An info metric: the value is always 1, the labels carry the content. It is the standard
	// way to get text into Prometheus — and at the same time the only way to spot a node on a
	// **different network** on the dashboard: when the genesis does not match the others, it is
	// not our chain.
	genesisID := n.chain.GenesisID()
	g("powm_node_info", "the node's network, label, version and genesis (the value is always 1)", 1,
		label("network", n.params.Name),
		label("node", n.nodeName),
		label("agent", n.agent),
		label("genesis", hex.EncodeToString(genesisID[:])))

	hdr, height, _ := n.chain.Tip()
	g("powm_tip_height", "height of the active branch", float64(height))
	// The age of the tip is the metric an alert should stand on: when the chain stops, it grows.
	g("powm_tip_age_seconds", "seconds since the last block",
		now.Sub(time.Unix(int64(hdr.Time), 0)).Seconds())

	tip, err := n.store.Tip()
	if err == nil {
		g("powm_utxo_applied_height", "the height the UTXO set is finished up to", float64(tip.Height))
		// Headers run ahead of bodies for a moment on every block — that is normal. A gap that
		// does not close is not: the UTXO set, and with it every wallet and the explorer, stops
		// at the hole while the headers keep growing. One missing body can hold every node
		// minutes behind, and without this gauge nothing shows it.
		gap := float64(0)
		if height > tip.Height {
			gap = float64(height - tip.Height)
		}
		g("powm_header_body_gap", "how many blocks the UTXO set is behind the headers", gap)
	}
	g("powm_utxo_count", "number of unspent outputs", float64(n.coinCount()))

	fh, _, fok := n.Finality()
	g("powm_finality_height", "highest finalized height", float64(fh))
	g("powm_finality_ok", "1 = the quorum has finalized something", bl(fok))
	if st := n.FinalityStatus(); st.Members > 0 {
		// The block on its way: how much of its quorum has signed and how much of it is this node.
		// A node whose own share is large and stays unsigned is a pool whose signing broke.
		g("powm_finality_members", "keys in the quorum of the block at the finality depth", float64(st.Members))
		g("powm_finality_signed_ratio", "share of the quorum's weight that signed the block at the finality depth",
			float64(st.Signed)/float64(st.Total))
		g("powm_finality_own_ratio", "share of the quorum's weight held by this node's key", float64(st.Own)/float64(st.Total))
	}
	g("powm_trusted_work", "1 = the chain meets the network's MinChainWork", bl(n.chain.TrustedWork()))

	// Chainwork is larger than a float64, so its logarithm is reported — that is enough for a
	// graph and it is not used for alerts.
	if w := n.chain.Work(); w != nil && w.Sign() > 0 {
		g("powm_chainwork_log2", "log2 of the cumulative chainwork", log2Big(w))
	}

	g("powm_peers", "number of connections", float64(len(n.connSnapshot())))

	// The body driver from the outside. Without these the only visible symptom of a body that
	// never arrives is a chain that mysteriously stops applying.
	bodyAge, inflight := n.bodies.oldest(now)
	g("powm_bodies_inflight", "block bodies requested and not yet delivered", float64(inflight))
	g("powm_oldest_missing_body_seconds", "how long the body waited for longest has been requested",
		bodyAge.Seconds())
	bSent, bDelivered, bTimeouts, bMismatch := n.bodies.counters()
	c("powm_body_requests_total", "body requests sent since the node started", float64(bSent))
	c("powm_bodies_delivered_total", "bodies delivered since the node started", float64(bDelivered))
	c("powm_body_timeouts_total", "body requests that ran into their timeout", float64(bTimeouts))
	c("powm_body_mismatch_total", "bodies that did not match the manifest", float64(bMismatch))

	mpCount, mpBytes := n.MempoolStats()
	g("powm_mempool_txs", "transactions in the mempool", float64(mpCount))
	g("powm_mempool_bytes", "size of the mempool in bytes", float64(mpBytes))
	g("powm_orphan_txs", "transactions waiting for a parent", float64(n.orphans.Len()))
	g("powm_orphan_bytes", "size of the orphan pool in bytes", float64(n.orphans.Bytes()))
	g("powm_sharepool_shares", "verified shares a block could still settle", float64(n.shares.Len()))

	st := n.chain.LaneStats()
	// The window is the longest one (chain.LaneWindows, 480 blocks today) and `fill` is how
	// full it really is: a young chain does not have 480 blocks, and a share computed against
	// the nominal window size would lie.
	win := len(chain.LaneWindows) - 1
	g("powm_lane_window_blocks", "size of the window the lane shares are computed from",
		float64(chain.LaneWindows[win]))
	if win < len(st.WindowFill) {
		g("powm_lane_window_fill", "how many blocks are really in that window", float64(st.WindowFill[win]))
	}
	for _, l := range st.Lanes {
		lab := `lane="` + l.Lane.String() + `"`
		// This is the metric the alert on a dead lane stands on. Spacing CANNOT be used for
		// it: a lane with fewer than two blocks in the window reports a spacing of 0 — so a
		// dead lane looks infinitely fast and a "spacing > X" threshold never fires.
		if win < len(l.BlocksInWindow) {
			g("powm_lane_blocks_in_window", "blocks of the lane in the window (0 = the lane is not mining)",
				float64(l.BlocksInWindow[win]), lab)
		}
		if l.HashrateEst != nil {
			f, _ := new(big.Float).SetInt(l.HashrateEst).Float64()
			g("powm_lane_hashrate", "estimated lane hashrate (H/s)", f, lab)
		}
		// A spacing of zero is not a spacing, it is "nothing to compute it from" — only a real
		// value is sent to the graph.
		if l.AvgSpacingSec > 0 {
			g("powm_lane_spacing_seconds", "average spacing of the lane's blocks", l.AvgSpacingSec, lab)
		}
		if l.LastTime != 0 {
			g("powm_lane_last_block_age_seconds", "seconds since the lane's last block",
				now.Sub(time.Unix(int64(l.LastTime), 0)).Seconds(), lab)
		}
	}

	skew, samples := n.ClockSkew()
	g("powm_time_offset_seconds", "offset of the machine's clock from the network", skew.Seconds())
	g("powm_time_peers", "how many peers the offset is computed from", float64(samples))

	g("powm_dag_full", "1 = the node holds the full DAG", bl(n.epochs.isFull()))
	g("powm_epoch", "the DAG epoch at the tip", float64(mh.EpochForHeight(height)))
	g("powm_finality_depth", "finality depth in blocks", float64(chain.FinalityDepth))
	n.stats.write(&b, n)
	return b.String()
}

// label assembles one Prometheus label. The value is escaped — the node label is validated
// (`ValidateNodeName`), but escaping it is cheaper than relying on that.
func label(name, value string) string {
	v := strings.ReplaceAll(value, `\`, `\\`)
	v = strings.ReplaceAll(v, `"`, `\"`)
	v = strings.ReplaceAll(v, "\n", `\n`)
	return name + `="` + v + `"`
}

// log2Big is the logarithm of a big number — chainwork does not fit into a float64.
func log2Big(x *big.Int) float64 {
	mant := new(big.Float)
	exp := new(big.Float).SetInt(x).MantExp(mant)
	m, _ := mant.Float64()
	return float64(exp) + math.Log2(m)
}
