package explorer

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"
)

// fakeNode is a node with fixed answers. The templates and the routes are tested, not the chain.
func fakeNode(t *testing.T, overrides map[string]any) *httptest.Server {
	t.Helper()
	resp := map[string]any{
		"getinfo": map[string]any{
			"network": "regtest", "height": 25, "utxo_count": 50,
			"mempool_txs": 0, "epoch": 0, "tip_time": time.Now().Unix(),
		},
		"getblocks": map[string]any{
			"height": 25,
			"blocks": []any{
				map[string]any{"id": strings.Repeat("a", 64), "height": 25, "lane": "G",
					"time": time.Now().Unix(), "bits": "2000ffff", "tx_count": 1,
					"size": 218, "value_out_cogs": 40000000},
				map[string]any{"id": strings.Repeat("b", 64), "height": 24, "lane": "C",
					"time": time.Now().Unix(), "bits": "2000ffff", "tx_count": 1,
					"size": 218, "value_out_cogs": 40000000},
			},
		},
		"getlanestats": map[string]any{
			"height": 25, "windows": []int{10, 40},
			"run": map[string]any{"lane": "G", "length": 1, "max_same_lane_run": 2},
			"lanes": []any{
				map[string]any{"lane": "C", "bits": "2000ffff", "work_per_block": "256",
					"hashrate_est": "1234567", "avg_spacing_s": 241, "target_spacing_s": 240,
					"last_block_height": 24, "last_block_time": time.Now().Unix(),
					"share": map[string]any{
						"last_10": map[string]any{"blocks": 5, "of_blocks": 10, "share_permille": 500},
						"last_40": map[string]any{"blocks": 12, "of_blocks": 25, "share_permille": 480},
					}},
				map[string]any{"lane": "G", "bits": "2000ffff", "work_per_block": "256",
					"hashrate_est": "0", "avg_spacing_s": 0, "target_spacing_s": 240,
					"last_block_height": 25, "last_block_time": time.Now().Unix(),
					"share": map[string]any{
						"last_10": map[string]any{"blocks": 5, "of_blocks": 10, "share_permille": 500},
						"last_40": map[string]any{"blocks": 13, "of_blocks": 25, "share_permille": 520},
					}},
			},
		},
		"getfinality": map[string]any{"finalized": true, "height": 20, "block": strings.Repeat("c", 64)},
		"gethashrate": map[string]any{
			"window_blocks": 12,
			"lanes": []any{
				map[string]any{"lane": "C", "points": []any{
					map[string]any{"t": 1_800_000_000, "hs": "12000", "h": 900},
					map[string]any{"t": 1_800_003_600, "hs": "15000", "h": 930},
					map[string]any{"t": 1_800_007_200, "hs": "14000", "h": 961},
				}},
				map[string]any{"lane": "G", "points": []any{
					map[string]any{"t": 1_800_000_000, "hs": "900000000", "h": 901},
					map[string]any{"t": 1_800_003_600, "hs": "1100000000", "h": 931},
					map[string]any{"t": 1_800_007_200, "hs": "1050000000", "h": 962},
				}},
			},
		},
		"getblock": map[string]any{
			"id": strings.Repeat("b", 64), "height": 24, "lane": "C", "time": time.Now().Unix(),
			"bits": "2000ffff", "version": 1, "merkle_root": strings.Repeat("d", 64),
			"nonce": 187, "prev_block": strings.Repeat("e", 64), "next_block": strings.Repeat("a", 64),
			"work": "256", "confirmations": 2, "size": 218,
			"txs": []string{strings.Repeat("f", 64)},
		},
		"getrawtransaction": map[string]any{
			"txid": strings.Repeat("f", 64), "block": strings.Repeat("b", 64), "height": 24,
			"index": 0, "time": time.Now().Unix(), "lane": "C", "confirmations": 2,
			"coinbase": true, "size": 134, "value_out_cogs": 40000000,
			"vin":  []any{map[string]any{"coinbase": true}},
			"vout": []any{map[string]any{"n": 0, "value_cogs": 38000000, "address": "rpowm1qtest"}},
		},
		"getaddress": map[string]any{
			"address": "rpowm1qtest", "balance_cogs": 950000000, "received_cogs": 950000000,
			"sent_cogs": 0, "tx_count": 25,
			"txs": []any{map[string]any{"txid": strings.Repeat("f", 64), "height": 24,
				"net_cogs": 38000000, "in_cogs": 38000000, "out_cogs": 0, "direction": "in"}},
		},
		"getrichlist": map[string]any{
			"supply_cogs": 1000000000,
			"addresses": []any{map[string]any{"rank": 1, "address": "rpowm1qtest",
				"balance_cogs": 950000000, "share_permille": 950}},
		},
	}
	for k, v := range overrides {
		if v == nil {
			// nil means "the node does not know this method" — the key is removed, not zeroed.
			delete(resp, k)
			continue
		}
		resp[k] = v
	}

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		out := map[string]any{"jsonrpc": "2.0", "id": 1}
		if v, ok := resp[req.Method]; ok {
			out["result"] = v
		} else {
			out["error"] = map[string]any{"code": -32000, "message": "unknown method"}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	}))
}

func newTestServer(t *testing.T, node *httptest.Server) http.Handler {
	t.Helper()
	srv, err := NewServer(NewClient(node.URL, "", 5*time.Second), Config{BasePath: "/explorer"})
	if err != nil {
		t.Fatal(err)
	}
	return srv.Handler()
}

func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

// Every page has to render and has to carry what a person came to it for.
//
// A Go template **does not crash** on a bad expression — it prints what it can. The address detail
// printed the whole struct including every transaction instead of the address this way, and the HTTP
// code was 200.
// That is why the content is checked here, not just the status.
func TestPagesRender(t *testing.T) {
	node := fakeNode(t, nil)
	defer node.Close()
	h := newTestServer(t, node)

	cases := []struct {
		path string
		want []string
		bad  []string
	}{
		// The front page: the ribbon of blocks with the full algorithm name, the chart, the table.
		{"/explorer/", []string{"ribbon-track", ">MachinaHash-C<", ">MachinaHash-G<", "Latest blocks"},
			[]string{">Lane C<", ">Lane G<"}},
		{"/explorer/blocks", []string{"Blocks", "aaaaaaaa…aaaaaaaa"}, nil},
		{"/explorer/algorithms", []string{"MachinaHash-C", "MachinaHash-G", "don't add up"}, nil},
		{"/explorer/block/24", []string{"Block 24", "187", "coinbase"}, nil},
		{"/explorer/tx/" + strings.Repeat("f", 64), []string{"Transaction", "0.40000000 POWM"}, nil},
		{"/explorer/txs", []string{"Transactions", "coinbase"}, nil},
		// The address has to be printed as an address, not as a dump of a struct.
		{"/explorer/address/rpowm1qtest",
			[]string{"rpowm1qtest", "9.50000000 POWM", "received"},
			[]string{"balance_cogs", "{rpowm1qtest"}},
		{"/explorer/richlist", []string{"rpowm1qtest", "95.0%"}, nil},
	}
	for _, c := range cases {
		rec := get(t, h, c.path)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status %d", c.path, rec.Code)
			continue
		}
		body := rec.Body.String()
		for _, w := range c.want {
			if !strings.Contains(body, w) {
				t.Errorf("%s: the page is missing %q", c.path, w)
			}
		}
		for _, b := range c.bad {
			if strings.Contains(body, b) {
				t.Errorf("%s: the page contains %q — the template printed an internal struct", c.path, b)
			}
		}
	}
}

// The search has to tell a height, a hash and an address apart without the visitor having to decide.
func TestSearchRoutes(t *testing.T) {
	node := fakeNode(t, nil)
	defer node.Close()
	h := newTestServer(t, node)

	cases := map[string]string{
		"24":                    "/explorer/block/24",
		strings.Repeat("b", 64): "/explorer/block/" + strings.Repeat("b", 64),
		"rpowm1qtest":           "/explorer/address/rpowm1qtest",
	}
	for q, want := range cases {
		rec := get(t, h, "/explorer/search?q="+q)
		if rec.Code != http.StatusSeeOther {
			t.Errorf("%q: status %d", q, rec.Code)
			continue
		}
		if got := rec.Header().Get("Location"); got != want {
			t.Errorf("%q → %s, expected %s", q, got, want)
		}
	}
}

// A hash that is not a block is a transaction — otherwise half the searches would end up as 404s.
func TestSearchFallsBackToTx(t *testing.T) {
	// A node that does not know the block with this id.
	node := fakeNode(t, map[string]any{"getblock": nil})
	defer node.Close()
	h := newTestServer(t, node)

	id := "9e" + strings.Repeat("a", 62)
	rec := get(t, h, "/explorer/search?q="+id)
	if got := rec.Header().Get("Location"); got != "/explorer/tx/"+id {
		t.Fatalf("the search ended at %s, expected a transaction", got)
	}
}

// When the node does not answer, the explorer has to say so and return 502 — not pretend an empty chain.
func TestNodeDownIsBadGateway(t *testing.T) {
	node := fakeNode(t, nil)
	h := newTestServer(t, node)
	node.Close()

	rec := get(t, h, "/explorer/")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status %d, expected 502", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "node is not responding") {
		t.Fatal("the page does not say the problem is on the node's side")
	}
}

// An unknown transaction is a 404, not a 502 — from a visitor's point of view it is a typo, not an outage.
func TestUnknownTxIsNotFound(t *testing.T) {
	node := fakeNode(t, map[string]any{"getrawtransaction": nil})
	defer node.Close()
	h := newTestServer(t, node)

	rec := get(t, h, "/explorer/tx/"+strings.Repeat("0", 64))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d, expected 404", rec.Code)
	}
}

// getWith is get with cookies.
func getWith(t *testing.T, h http.Handler, path string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// Switching the theme has to work without JavaScript: the link stores the choice and redirects back to
// a clean address, so it can be passed on.
func TestThemeSwitch(t *testing.T) {
	node := fakeNode(t, nil)
	defer node.Close()
	h := newTestServer(t, node)

	rec := get(t, h, "/explorer/?theme=dark")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status %d, expected 303", rec.Code)
	}
	if got := rec.Header().Get("Location"); got != "/explorer/" {
		t.Fatalf("it redirected to %q — the theme parameter should have disappeared from the address", got)
	}
	var set *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == "theme" {
			set = c
		}
	}
	if set == nil || set.Value != "dark" {
		t.Fatalf("the theme cookie was not set: %+v", set)
	}

	// With the cookie the theme is written on <html>.
	body := getWith(t, h, "/explorer/", set).Body.String()
	if !strings.Contains(body, `data-theme="dark"`) {
		t.Fatal("the page does not carry data-theme, the theme would not take effect")
	}
}

// Without a choice, data-theme is **not written at all** — then the system setting applies.
// If we wrote "light" there unconditionally, everybody with a dark system would get a light page.
func TestThemeDefaultsToSystem(t *testing.T) {
	node := fakeNode(t, nil)
	defer node.Close()
	h := newTestServer(t, node)

	body := get(t, h, "/explorer/").Body.String()
	if strings.Contains(body, "data-theme") {
		t.Fatal("without a choice there must be no data-theme — otherwise prefers-color-scheme is ignored")
	}
}

// A nonsensical value must not be stored; the address is cleaned up anyway.
func TestThemeRejectsUnknownValue(t *testing.T) {
	node := fakeNode(t, nil)
	defer node.Close()
	h := newTestServer(t, node)

	rec := get(t, h, "/explorer/?theme=neon")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status %d, expected 303", rec.Code)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == "theme" {
			t.Fatalf("an unknown theme %q was stored", c.Value)
		}
	}
}

// Switching the theme must not drop the other parameters — otherwise somebody on the third page of a
// list would end up on the first after switching.
func TestThemeKeepsOtherParams(t *testing.T) {
	node := fakeNode(t, nil)
	defer node.Close()
	h := newTestServer(t, node)

	body := get(t, h, "/explorer/blocks?from=20").Body.String()
	if !strings.Contains(body, "from=20&amp;theme=dark") {
		t.Fatal("the theme switch link lost the from parameter")
	}

	rec := get(t, h, "/explorer/blocks?from=20&theme=dark")
	if got := rec.Header().Get("Location"); got != "/explorer/blocks?from=20" {
		t.Fatalf("after switching the theme the address is %q — from was lost", got)
	}
}

// The cache must not mix themes between visitors.
func TestThemedPagesVaryOnCookie(t *testing.T) {
	node := fakeNode(t, nil)
	defer node.Close()
	h := newTestServer(t, node)

	rec := get(t, h, "/explorer/")
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "private") {
		t.Fatalf("Cache-Control %q — a shared cache would hand out somebody else's theme", cc)
	}
	if v := rec.Header().Get("Vary"); !strings.Contains(v, "Cookie") {
		t.Fatalf("Vary %q, expected Cookie", v)
	}
}

// The stylesheet's address has to carry a fingerprint of its content.
//
// A CDN in front of the explorer caches .css by itself. After a new version is deployed it would
// serve the old stylesheet with the new HTML — the header falls apart and it looks as if the CSS were
// broken. A fingerprint in the address solves that without purging the cache on every deployment.
func TestStylesheetURLIsContentAddressed(t *testing.T) {
	node := fakeNode(t, nil)
	defer node.Close()
	h := newTestServer(t, node)

	body := get(t, h, "/explorer/").Body.String()
	m := regexp.MustCompile(`/explorer/style\.[0-9a-f]{8}\.css`).FindString(body)
	if m == "" {
		t.Fatal("the page links to a stylesheet without a fingerprint — a CDN will hand out the old one")
	}

	rec := get(t, h, m)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s: status %d", m, rec.Code)
	}
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Fatalf("Cache-Control %q — an address with a fingerprint may be cached forever", cc)
	}
}

// The global header is shared with the site and has to be on every page of the explorer — including
// the error one. The section tabs have to mark the one the visitor is on.
func TestSharedHeaderAndTabs(t *testing.T) {
	node := fakeNode(t, nil)
	defer node.Close()
	h := newTestServer(t, node)

	cases := map[string]string{
		"/explorer/":                    "Overview",
		"/explorer/blocks":              "Blocks",
		"/explorer/block/24":            "Blocks",
		"/explorer/txs":                 "Transactions",
		"/explorer/algorithms":          "Algorithms",
		"/explorer/richlist":            "Rich list",
		"/explorer/address/rpowm1qtest": "Rich list",
	}
	for path, tab := range cases {
		body := get(t, h, path).Body.String()
		for _, want := range []string{
			`href="/site-header.css"`, `class="site-header"`,
			`>How it works</a>`, `>Explorer</a>`, `href="https://pool.powmachina.com/" rel="noopener">Pool</a>`, `>Wallet</a>`, `Whitepaper<i>soon</i>`, `href="/download/">Download</a>`,
			`data-set-theme="dark"`,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("%s: the shared header is missing %q", path, want)
			}
		}
		if !strings.Contains(body, `aria-current="page">`+tab+`</a>`) {
			t.Errorf("%s: the active tab is not %q", path, tab)
		}
	}

	// The error page has a header too — somebody who hits a typo has to be able to get back.
	node2 := fakeNode(t, map[string]any{"getrawtransaction": nil})
	defer node2.Close()
	body := get(t, newTestServer(t, node2), "/explorer/tx/"+strings.Repeat("0", 64)).Body.String()
	if !strings.Contains(body, `class="site-header"`) {
		t.Error("the error page has no shared header")
	}
	// And it must not show the node's raw message, which is written for the operator.
	if strings.Contains(body, "unknown method") {
		t.Error("the error page shows the node's raw error")
	}
}

// The theme cookie has to be readable for /theme.js — otherwise a choice made in the explorer would
// not work on the static site.
func TestThemeCookieReadableByScript(t *testing.T) {
	node := fakeNode(t, nil)
	defer node.Close()
	h := newTestServer(t, node)

	for _, c := range get(t, h, "/explorer/?theme=dark").Result().Cookies() {
		if c.Name == "theme" && c.HttpOnly {
			t.Fatal("the theme cookie is HttpOnly — theme.js on the site cannot read it")
		}
	}
}

// Without the txindex the transaction list is not "empty" — the node merely cannot look them up.
// The page has to say that truthfully and must not fail with a 502.
func TestTxsWithoutIndexSaysSo(t *testing.T) {
	node := fakeNode(t, map[string]any{"getrawtransaction": nil})
	defer node.Close()
	h := newTestServer(t, node)

	rec := get(t, h, "/explorer/txs")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d — a missing index is not an outage", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "No transactions yet") {
		t.Fatal("the page claims there are no transactions, although the node merely does not index them")
	}
	if !strings.Contains(body, "-txindex") {
		t.Fatal("the page does not say what is missing")
	}
}

// /lanes is the other name people reach for. It has to redirect to /algorithms rather than
// return a 404.
func TestLanesRedirectsToAlgorithms(t *testing.T) {
	node := fakeNode(t, nil)
	defer node.Close()
	srv := newTestServer(t, node)
	rec := get(t, srv, "/explorer/lanes")
	if rec.Code != http.StatusMovedPermanently {
		t.Fatalf("/explorer/lanes returned %d, expected 301", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/explorer/algorithms" {
		t.Fatalf("a redirect to %q", loc)
	}
}

// The hashrate chart on the front page: the SVG is rendered on the server, both lanes have a curve and
// their last value in the legend.
func TestHomeHashrateChart(t *testing.T) {
	node := fakeNode(t, nil)
	defer node.Close()
	srv := newTestServer(t, node)
	body := get(t, srv, "/explorer/").Body.String()
	for _, want := range []string{"Network hashrate", "<svg", "class=\"band lane-C\"",
		"class=\"band lane-G\"", "MachinaHash-C", "<b>14.0 kH/s</b>", "<b>1.0 GH/s</b>",
		">MachinaHash-C <", ">MachinaHash-G <", "the same step for both", "2 hours"} {
		if !strings.Contains(body, want) {
			t.Fatalf("the chart is missing %q", want)
		}
	}
}

// The chart can be read with the mouse too: the strips carry the time, both lanes' values and a link to
// the block. All without JavaScript, so it has to be in the HTML rather than computed in the browser.
func TestHomeChartTips(t *testing.T) {
	node := fakeNode(t, nil)
	defer node.Close()
	srv := newTestServer(t, node)
	body := get(t, srv, "/explorer/").Body.String()
	for _, want := range []string{`class="hit"`, `class="guide"`, `class="tip-box"`,
		`aria-label="Block 962 `, "MachinaHash-G 1.0 GH/s", "MachinaHash-C 14.0 kH/s", "block 962 →"} {
		if !strings.Contains(body, want) {
			t.Fatalf("the chart is missing %q", want)
		}
	}
	if n := strings.Count(body, `class="hit"`); n < 2 {
		t.Fatalf("there are %d strips for the mouse", n)
	}
	// The chart stands above the ribbon of blocks — it is the page's main element.
	if strings.Index(body, `class="chart"`) > strings.Index(body, `class="ribbon"`) {
		t.Fatal("the chart is not above the block ribbon")
	}
}

// The chart's range switcher: a link in the URL picks the range and the page renders with it marked.
// It works without JavaScript — the explorer has none.
func TestHomeChartRangeSwitch(t *testing.T) {
	node := fakeNode(t, nil)
	defer node.Close()
	srv := newTestServer(t, node)

	body := get(t, srv, "/explorer/").Body.String()
	if !strings.Contains(body, `href="?range=24h"`) {
		t.Fatal("the range switcher is missing from the page")
	}
	if !strings.Contains(body, `href="?range=6h" class="on"`) {
		t.Fatal("without a choice the default range should be marked")
	}
	body = get(t, srv, "/explorer/?range=24h").Body.String()
	if !strings.Contains(body, `href="?range=24h" class="on"`) {
		t.Fatal("the chosen range is not marked")
	}
	// An invented value in the URL must not break the page.
	body = get(t, srv, "/explorer/?range=nonsense").Body.String()
	if !strings.Contains(body, `href="?range=6h" class="on"`) {
		t.Fatal("an unknown range did not fall back to the default")
	}
}

// A node without the chart (a young chain or an older version) — the page renders without it.
func TestHomeWithoutChart(t *testing.T) {
	node := fakeNode(t, map[string]any{"gethashrate": map[string]any{"window_blocks": 12, "lanes": []any{}}})
	defer node.Close()
	srv := newTestServer(t, node)
	body := get(t, srv, "/explorer/").Body.String()
	if strings.Contains(body, "Network hashrate") {
		t.Fatal("an empty chart should not be drawn")
	}
	if !strings.Contains(body, "Latest blocks") {
		t.Fatal("the rest of the page is missing")
	}
}

// The ribbon of blocks: one chip per block, the newest on the left, a link to the detail and the full
// algorithm name (not the lane's letter).
func TestHomeRibbon(t *testing.T) {
	node := fakeNode(t, nil)
	defer node.Close()
	body := get(t, newTestServer(t, node), "/explorer/").Body.String()
	ribbon := body[strings.Index(body, `<ol class="ribbon-track">`):strings.Index(body, "</ol>")]
	if n := strings.Count(ribbon, `class="chip lane-`); n != 2 {
		t.Fatalf("there are %d chips in the ribbon, there are 2 blocks", n)
	}
	first := strings.Index(ribbon, "25")
	second := strings.Index(ribbon, "24")
	if first < 0 || second < 0 || first > second {
		t.Fatal("the newest block is not first")
	}
	for _, want := range []string{`href="/explorer/block/25"`, ">MachinaHash-G<", ">MachinaHash-C<", "tx ·"} {
		if !strings.Contains(ribbon, want) {
			t.Fatalf("the ribbon is missing %q", want)
		}
	}
	if strings.Contains(ribbon, ">C<") || strings.Contains(ribbon, ">G<") {
		t.Fatal("the ribbon has the bare lane letter instead of the algorithm name")
	}
}
