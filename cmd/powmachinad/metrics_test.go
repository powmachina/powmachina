package main

import (
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"powmachina/internal/chain"
	"powmachina/internal/mh"
)

// A scrape is parsed whole or not at all: one bad header throws away every metric, not just
// its own line. So the test walks the output the way Prometheus walks it.
func parseMetrics(t *testing.T, out string) map[string]float64 {
	t.Helper()
	vals := make(map[string]float64)
	help := make(map[string]bool)
	typ := make(map[string]bool)
	for i, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		where := "line " + strconv.Itoa(i+1) + " (" + line + ")"
		switch {
		case strings.HasPrefix(line, "# HELP "):
			f := strings.SplitN(strings.TrimPrefix(line, "# HELP "), " ", 2)
			if len(f) != 2 || f[1] == "" {
				t.Fatalf("%s: HELP with no text", where)
			}
			if help[f[0]] {
				t.Fatalf("%s: a second HELP header for %s — Prometheus throws away the whole scrape over this", where, f[0])
			}
			help[f[0]] = true
		case strings.HasPrefix(line, "# TYPE "):
			f := strings.SplitN(strings.TrimPrefix(line, "# TYPE "), " ", 2)
			if len(f) != 2 {
				t.Fatalf("%s: TYPE with no type", where)
			}
			if typ[f[0]] {
				t.Fatalf("%s: a second TYPE header for %s", where, f[0])
			}
			typ[f[0]] = true
		case line == "":
		default:
			f := strings.Fields(line)
			if len(f) != 2 {
				t.Fatalf("%s: expected `name value`", where)
			}
			name := f[0]
			if i := strings.IndexByte(name, '{'); i >= 0 {
				if !strings.HasSuffix(name, "}") {
					t.Fatalf("%s: unclosed label", where)
				}
				name = name[:i]
			}
			// A histogram's samples carry suffixes on the name its HELP/TYPE gave.
			for _, suf := range []string{"_bucket", "_sum", "_count"} {
				if base := strings.TrimSuffix(name, suf); base != name && typ[base] {
					name = base
				}
			}
			if !help[name] || !typ[name] {
				t.Fatalf("%s: metric %s has no HELP/TYPE before its first value", where, name)
			}
			v, err := strconv.ParseFloat(f[1], 64)
			if err != nil {
				t.Fatalf("%s: the value is not a number: %v", where, err)
			}
			vals[f[0]] = v
		}
	}
	return vals
}

func TestMetricsScrapeIsWellFormed(t *testing.T) {
	node := NewNode(chain.RegTest, chain.RegtestGenesis(), mh.TestProfile)
	defer node.Close()
	if err := node.MineBlocks(4); err != nil {
		t.Fatal(err)
	}

	vals := parseMetrics(t, node.metrics(time.Now()))
	for _, want := range []string{
		"powm_tip_height", "powm_tip_age_seconds", "powm_utxo_count",
		"powm_finality_height", "powm_finality_ok", "powm_trusted_work",
		"powm_peers", "powm_mempool_txs", "powm_mempool_bytes",
		"powm_time_offset_seconds", "powm_epoch", "powm_finality_depth",
		`powm_shares_received_total{result="accepted"}`, `powm_blocks_submitted_total{result="stale"}`,
		"powm_reorgs_total", "powm_sibling_switches_total", "powm_block_process_seconds_count",
		"powm_tip_seen_timestamp_seconds", "powm_tip_settled_units", "powm_credit_keys",
	} {
		if _, ok := vals[want]; !ok {
			t.Errorf("%s is missing from the scrape", want)
		}
	}
	// The info metric has to carry the network and the genesis: they are how a node running on a
	// different network is spotted on the dashboard (same magic + a different root = a silent split).
	gen := node.chain.GenesisID()
	want := `powm_node_info{network="regtest",node="",agent="` + node.agent +
		`",genesis="` + hex.EncodeToString(gen[:]) + `"}`
	if v, ok := vals[want]; !ok || v != 1 {
		t.Errorf("%s is missing (got %v)", want, vals[want])
	}
	if vals["powm_tip_height"] != 4 {
		t.Errorf("tip_height = %v, expected 4", vals["powm_tip_height"])
	}
	if vals["powm_finality_depth"] != float64(chain.FinalityDepth) {
		t.Errorf("finality_depth = %v", vals["powm_finality_depth"])
	}
	// Every block the node mined went through processing once.
	if vals["powm_block_process_seconds_count"] != 4 || vals[`powm_block_process_seconds_bucket{le="+Inf"}`] != 4 {
		t.Errorf("block_process_seconds counted %v blocks, expected 4", vals["powm_block_process_seconds_count"])
	}
}

// Lane metrics have a label, so the same name is printed twice — this is exactly where a
// duplicate HELP header shows up if somebody removes it from `seen`.
func TestMetricsLanesShareOneHeader(t *testing.T) {
	node := NewNode(chain.RegTest, chain.RegtestGenesis(), mh.TestProfile)
	defer node.Close()
	if err := node.MineBlocks(6); err != nil {
		t.Fatal(err)
	}
	out := node.metrics(time.Now())
	vals := parseMetrics(t, out)

	for _, lane := range []mh.Lane{mh.LaneC, mh.LaneG} {
		key := `powm_lane_blocks_in_window{lane="` + lane.String() + `"}`
		if vals[key] == 0 {
			t.Errorf("%s = 0, even though the lane was mining:\n%s", key, out)
		}
	}
}

// The alert on a dead lane stands on a dead lane reporting **zero** and not nothing: if the
// metric were not printed at all for a lane with no blocks, `< 5` would have nothing to catch.
// (Spacing cannot be used for it — for a dead lane that is zero, i.e. "infinitely fast".)
func TestMetricsDeadLaneReportsZero(t *testing.T) {
	node := NewNode(chain.RegTest, chain.RegtestGenesis(), mh.TestProfile)
	defer node.Close()

	vals := parseMetrics(t, node.metrics(time.Now()))
	for _, lane := range []mh.Lane{mh.LaneC, mh.LaneG} {
		key := `powm_lane_blocks_in_window{lane="` + lane.String() + `"}`
		v, ok := vals[key]
		if !ok {
			t.Fatalf("%s was not printed — the alert on a dead lane would have nothing to measure", key)
		}
		if v != 0 {
			t.Errorf("%s = %v, expected 0", key, v)
		}
	}
	// The alert threshold is conditioned on how full the window is, so that has to be in the
	// scrape too — otherwise `and on (instance) powm_lane_window_fill >= 60` silences the whole
	// alert for good.
	if _, ok := vals["powm_lane_window_fill"]; !ok {
		t.Error("powm_lane_window_fill is missing")
	}
	if vals["powm_lane_window_blocks"] == 0 {
		t.Error("powm_lane_window_blocks is missing")
	}
}

// The alert on a stalled chain stands on the age of the tip growing with real time. If it were
// computed from something else (for instance from when the node received the block), a stalled
// chain would look healthy.
func TestMetricsTipAgeGrowsWithTime(t *testing.T) {
	node := NewNode(chain.RegTest, chain.RegtestGenesis(), mh.TestProfile)
	defer node.Close()
	if err := node.MineBlocks(1); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	first := parseMetrics(t, node.metrics(now))["powm_tip_age_seconds"]
	later := parseMetrics(t, node.metrics(now.Add(30*time.Minute)))["powm_tip_age_seconds"]
	if later-first < 1799 || later-first > 1801 {
		t.Fatalf("the age of the tip moved by %v s after 30 min (from %v to %v)", later-first, first, later)
	}
}

func TestMetricsHTTPEndpoint(t *testing.T) {
	node := NewNode(chain.RegTest, chain.RegtestGenesis(), mh.TestProfile)
	defer node.Close()

	rec := httptest.NewRecorder()
	node.metricsHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("Content-Type %q — Prometheus expects text/plain", ct)
	}
	parseMetrics(t, rec.Body.String())
}
