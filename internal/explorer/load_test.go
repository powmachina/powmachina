package explorer

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// countingNode is fakeNode that also counts the calls of each method.
func countingNode(t *testing.T, overrides map[string]any) (*httptest.Server, func(method string) int) {
	t.Helper()
	inner := fakeNode(t, overrides)
	t.Cleanup(inner.Close)
	var mu sync.Mutex
	calls := map[string]int{}
	outer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var req struct {
			Method string `json:"method"`
		}
		_ = json.Unmarshal(raw, &req)
		mu.Lock()
		calls[req.Method]++
		mu.Unlock()
		r.Body = io.NopCloser(bytes.NewReader(raw))
		inner.Config.Handler.ServeHTTP(w, r)
	}))
	return outer, func(method string) int {
		mu.Lock()
		defer mu.Unlock()
		return calls[method]
	}
}

// A block's page asks the node for each transaction it shows. A full block has hundreds of them, so
// the page has to show a stretch of the block and link to the rest — otherwise one request for a
// page is hundreds of calls to the node, and a handful of visitors keep it busy.
func TestBlockPageLoadsOnePageOfTransactions(t *testing.T) {
	const total = 320
	txs := make([]string, total)
	for i := range txs {
		txs[i] = fmt.Sprintf("%064x", i+1)
	}
	id := strings.Repeat("b", 64)
	block := map[string]any{
		"id": id, "height": 24, "lane": "C", "time": time.Now().Unix(),
		"bits": "2000ffff", "version": 1, "merkle_root": strings.Repeat("d", 64),
		"work": "256", "confirmations": 2, "size": 250000, "txs": txs,
	}
	node, calls := countingNode(t, map[string]any{"getblock": block})
	defer node.Close()
	h := newTestServer(t, node)

	rec := get(t, h, "/explorer/block/24")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if n := calls("getrawtransaction"); n > blockTxLimit {
		t.Fatalf("one page of a block of %d transactions made %d calls to the node, the cap is %d",
			total, n, blockTxLimit)
	}
	body := rec.Body.String()
	// The count is the block's, not the page's, and the rest has to be reachable.
	for _, want := range []string{"<dd>320</dd>", "1–50 of 320", `href="/explorer/block/` + id + `?txs=50"`} {
		if !strings.Contains(body, want) {
			t.Errorf("the first page is missing %q", want)
		}
	}
	if strings.Contains(body, "← previous") {
		t.Error("the first page links to a page before it")
	}

	// The last page is shorter and has nothing after it.
	before := calls("getrawtransaction")
	body = get(t, h, "/explorer/block/24?txs=300").Body.String()
	if n := calls("getrawtransaction") - before; n != 20 {
		t.Fatalf("the last page made %d calls, 20 transactions are left", n)
	}
	for _, want := range []string{"301–320 of 320", `href="/explorer/block/` + id + `?txs=250"`} {
		if !strings.Contains(body, want) {
			t.Errorf("the last page is missing %q", want)
		}
	}
	if strings.Contains(body, "next →") {
		t.Error("the last page links to a page after it")
	}

	// A position past the end, or one that is not a number, is the first page: not an error and not
	// more work for the node.
	for _, q := range []string{"99999", "-5", "abc", "9999999999999999999999"} {
		before := calls("getrawtransaction")
		rec := get(t, h, "/explorer/block/24?txs="+q)
		if rec.Code != http.StatusOK {
			t.Errorf("txs=%s: status %d", q, rec.Code)
		}
		if n := calls("getrawtransaction") - before; n > blockTxLimit {
			t.Errorf("txs=%s: %d calls to the node", q, n)
		}
		if !strings.Contains(rec.Body.String(), "1–50 of 320") {
			t.Errorf("txs=%s: the page is not the first one", q)
		}
	}
}

// A height, a hash and an address each have a shape, and what does not have it cannot match
// anything. The explorer says so itself: the value in the address of a page is typed by anybody,
// and passing it on would let anybody keep the node answering nonsense.
func TestMalformedPathValuesNeverReachTheNode(t *testing.T) {
	node, calls := countingNode(t, nil)
	defer node.Close()
	h := newTestServer(t, node)

	total := func() int {
		n := 0
		for _, m := range []string{"getblock", "getrawtransaction", "getaddress", "getinfo"} {
			n += calls(m)
		}
		return n
	}
	hash := strings.Repeat("f", 64)
	bad := []string{
		"/explorer/block/xyz",
		"/explorer/block/-1",
		"/explorer/block/+24",
		"/explorer/block/0x18",
		"/explorer/block/99999999999999999999",
		"/explorer/block/" + hash[:63],
		"/explorer/block/" + hash + "0",
		"/explorer/block/" + hash[:63] + "g",
		"/explorer/tx/24",
		"/explorer/tx/" + hash[:62],
		"/explorer/tx/" + hash[:63] + "%00",
		"/explorer/tx/" + strings.Repeat("f", 4096),
		"/explorer/address/rpowm1q%20test",
		"/explorer/address/rpowm1q%22%7D",
		"/explorer/address/rpowm1q%1b%5b2J",
		"/explorer/address/" + strings.Repeat("q", 91),
	}
	for _, path := range bad {
		rec := get(t, h, path)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: status %d, expected 404", path, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "Nothing matches that") {
			t.Errorf("%s: the page does not say that nothing matches", path)
		}
		if !strings.Contains(rec.Body.String(), `class="site-header"`) {
			t.Errorf("%s: the error page has no shared header", path)
		}
	}
	if n := total(); n != 0 {
		t.Fatalf("%d malformed addresses made %d calls to the node, expected none", len(bad), n)
	}

	// What has the right shape goes through as before, in either case of hex.
	good := []string{
		"/explorer/block/0",
		"/explorer/block/24",
		"/explorer/block/" + strings.Repeat("b", 64),
		"/explorer/block/" + strings.Repeat("B", 64),
		"/explorer/tx/" + hash,
		"/explorer/address/rpowm1qtest",
	}
	for _, path := range good {
		if rec := get(t, h, path); rec.Code != http.StatusOK {
			t.Errorf("%s: status %d", path, rec.Code)
		}
	}
	if calls("getblock") != 4 || calls("getaddress") != 1 {
		t.Fatalf("well-formed values did not reach the node: getblock %d, getaddress %d",
			calls("getblock"), calls("getaddress"))
	}
}

// A block that fits on one page has no pager — most blocks do.
func TestSmallBlockHasNoPager(t *testing.T) {
	node := fakeNode(t, nil)
	defer node.Close()
	body := get(t, newTestServer(t, node), "/explorer/block/24").Body.String()
	if strings.Contains(body, "?txs=") || strings.Contains(body, "1–1 of 1") {
		t.Fatal("a block of one transaction has a pager")
	}
}
