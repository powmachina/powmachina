package explorer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

//go:embed templates
var templateFS embed.FS

// Config is the explorer's configuration.
type Config struct {
	// BasePath is the prefix the explorer is served under — on powmachina.com it is "/explorer".
	// Every link in the templates goes through it, so moving it under another prefix is a change of one flag.
	BasePath string
	// Network is the network's name; it is shown in the header so nobody mistakes testnet for mainnet.
	Network string
	// BlocksPerPage is the length of the block list.
	BlocksPerPage int
	// TxsPerPage is the length of an address's history on one page.
	TxsPerPage int
	// Timeout is the cap on one request including the calls to the node.
	Timeout time.Duration
	// GeoNodes are more nodes whose share countries the SharePool map sums with the main node's:
	// the public nodes the network's shares come in through (getsharegeo).
	GeoNodes []*Client
}

func (c *Config) applyDefaults() {
	c.BasePath = "/" + strings.Trim(c.BasePath, "/")
	if c.BasePath == "/" {
		c.BasePath = ""
	}
	if c.BlocksPerPage <= 0 {
		c.BlocksPerPage = 25
	}
	if c.TxsPerPage <= 0 {
		c.TxsPerPage = 50
	}
	if c.Timeout <= 0 {
		c.Timeout = 10 * time.Second
	}
}

// Server is the explorer's HTTP interface.
type Server struct {
	cli  *Client
	cfg  Config
	tmpl *template.Template
	// css is the stylesheet's content and cssPath its address with a fingerprint of that content.
	//
	// The fingerprint is there because a CDN in front of the explorer caches .css by itself. After
	// a new version is deployed it would serve the old stylesheet with the new HTML and the page
	// would fall apart. A new address = a new file; purging the cache stops being part of a deployment.
	css     []byte
	cssPath string
	// live is the shared answer of the live API (live.go).
	live liveCache
}

func NewServer(cli *Client, cfg Config) (*Server, error) {
	cfg.applyDefaults()
	s := &Server{cli: cli, cfg: cfg}
	t := template.New("").Funcs(s.funcs())
	t, err := t.ParseFS(templateFS, "templates/*.html")
	if err != nil {
		return nil, err
	}
	s.tmpl = t

	css, err := templateFS.ReadFile("templates/style.css")
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(css)
	s.css = css
	s.cssPath = fmt.Sprintf("/style.%s.css", hex.EncodeToString(sum[:4]))
	return s, nil
}

func (s *Server) funcs() template.FuncMap {
	return template.FuncMap{
		"cogs":      FormatCogs,
		"cogsShort": FormatCogsShort,
		"hashrate":  FormatHashrate,
		"algo":      AlgoName,
		// addf/subf: the template draws an SVG chart and shifts coordinates by a few pixels
		"addf":      func(a, b float64) float64 { return a + b },
		"subf":      func(a, b float64) float64 { return a - b },
		"bytes":     FormatBytes,
		"time":      FormatTime,
		"permille":  Permille,
		"short":     Short,
		"bar":       Bar,
		"since":     func(u int64) string { return Since(u, time.Now()) },
		"url":       func(p string) string { return s.cfg.BasePath + p },
		"cssurl":    func() string { return s.cfg.BasePath + s.cssPath },
		"add":       func(a, b int) int { return a + b },
		"upper":     strings.ToUpper,
		"icon":      iconHTML,
		"mosaic":    Mosaic,
		"unitshare": unitShare,
	}
}

// Handler returns the explorer's router.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	b := s.cfg.BasePath
	mux.HandleFunc("GET "+b+"/{$}", s.handleHome)
	mux.HandleFunc("GET "+b+"/blocks", s.handleBlocks)
	mux.HandleFunc("GET "+b+"/txs", s.handleTxs)
	mux.HandleFunc("GET "+b+"/block/{id}", s.handleBlock)
	mux.HandleFunc("GET "+b+"/tx/{txid}", s.handleTx)
	mux.HandleFunc("GET "+b+"/address/{addr}", s.handleAddress)
	mux.HandleFunc("GET "+b+"/algorithms", s.handleLanes)
	// /lanes is an alias: the page is about the lanes, so the address gets typed and linked too.
	mux.HandleFunc("GET "+b+"/lanes", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, s.cfg.BasePath+"/algorithms", http.StatusMovedPermanently)
	})
	mux.HandleFunc("GET "+b+"/richlist", s.handleRichList)
	mux.HandleFunc("GET "+b+"/shares", s.handleShares)
	mux.HandleFunc("GET "+b+"/api/sharepool", s.handleLive)
	mux.HandleFunc("GET "+b+"/api/sharepool/miners", s.handleLiveMiners)
	mux.HandleFunc("GET "+b+"/search", s.handleSearch)
	mux.HandleFunc("GET "+b+s.cssPath, s.handleCSS)
	// The plain address works too, for anything that links the stylesheet without the fingerprint.
	mux.HandleFunc("GET "+b+"/style.css", s.handleCSS)
	if b != "" {
		// Without a trailing slash /explorer would end up as a 404.
		mux.HandleFunc("GET "+b, func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, b+"/", http.StatusMovedPermanently)
		})
	}
	return withTheme(mux)
}

// handleCSS serves the explorer's only static file. It is embedded in the binary, so a deployment is
// one file and not a directory that can diverge from the code.
func (s *Server) handleCSS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	// The address carries a fingerprint of the content, so it may be cached forever.
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	_, _ = w.Write(s.css)
}

// page is the common wrapper of every page.
type page struct {
	Title   string
	Network string
	Base    string
	Info    *Info
	Data    any
	// Tab is the active tab of the explorer's section ("overview", "blocks", ...).
	Tab string
	// Theme is the chosen theme, or empty when the system setting applies.
	Theme string
	// LightHref and DarkHref are the theme switcher's links for this address.
	LightHref string
	DarkHref  string
	// Err is the error message when the page cannot be assembled.
	Err string
}

// newPage assembles a page's wrapper. The theme and its switcher belong on every page including the
// error one — somebody who hits a 404 should be able to switch it too.
func (s *Server) newPage(r *http.Request, title string) page {
	return page{
		Title:     title,
		Network:   s.cfg.Network,
		Base:      s.cfg.BasePath,
		Theme:     themeOf(r),
		LightHref: themeLink(r, "light"),
		DarkHref:  themeLink(r, "dark"),
	}
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, name, title, tab string, data any) {
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.Timeout)
	defer cancel()

	p := s.newPage(r, title)
	p.Tab = tab
	p.Data = data
	var info Info
	if err := s.cli.Call(ctx, "getinfo", nil, &info); err == nil {
		p.Info = &info
		if info.Network != "" {
			p.Network = info.Network
		}
	}
	// The page is rendered whole before anything is sent. A template that fails half way otherwise
	// went out as half a page with status 200, and the error was lost.
	var buf bytes.Buffer
	if err := s.tmpl.ExecuteTemplate(&buf, name, p); err != nil {
		log.Printf("explorer: rendering %s: %v", name, err)
		http.Error(w, "The page could not be rendered.", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// The explorer is public and the page changes with every block. A short cache keeps the load on the
	// node in check without showing a stale tip.
	//
	// `private` + `Vary: Cookie`, because since the switcher was introduced the HTML depends on a
	// cookie — a shared cache would otherwise hand somebody else's theme to a visitor.
	w.Header().Set("Cache-Control", "private, max-age=10")
	w.Header().Set("Vary", "Cookie")
	_, _ = w.Write(buf.Bytes())
}

// fail renders the error page with the right status code.
//
// "the node does not know it" (404) is distinguished from "the node does not answer" (502). From a
// visitor's point of view that is the difference between a typo and an outage and the explorer must
// not blur it.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	status := http.StatusBadGateway
	msg := "The node is not responding. The explorer keeps no copy of the chain of its own, so there is nothing to show right now."
	var rpcErr *Err
	if errors.As(err, &rpcErr) {
		status = http.StatusNotFound
		// The node's error is not shown to a visitor verbatim: it is written for the operator (and in the
		// language of the code), not for a person who mistyped a txid.
		msg = "Nothing matches that. Check the height, hash or address and try again."
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.Timeout)
	defer cancel()
	p := s.newPage(r, "Not found")
	p.Err = msg
	// The status line of the network is an extra. A request that could not have matched anything is
	// answered without it, so it costs the node nothing at all.
	if err != errShape {
		var info Info
		if err := s.cli.Call(ctx, "getinfo", nil, &info); err == nil {
			p.Info = &info
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_ = s.tmpl.ExecuteTemplate(w, "error.html", p)
}

// errShape is the answer to a height, hash or address that cannot be one by its shape alone.
//
// The values come from the address of the page, typed by anybody. The node would refuse them too,
// but each refusal is a call to it: the explorer is public and the node behind it is not, so what
// can be decided here is decided here. Only the shape is checked — whether an address has a valid
// checksum or a hash names a block is the node's answer to give.
var errShape = &Err{Msg: "not a height, a hash or an address"}

// maxAddressLen is the longest address the node decodes (address.MaxLength).
const maxAddressLen = 90

// isHash says whether s has the shape of a block id or a txid: 32 bytes in hex.
func isHash(s string) bool { return len(s) == 64 && isHex(s) }

// isHeight says whether s has the shape of a height: a number that fits into a uint64.
func isHeight(s string) bool { return isDigits(s) && len(s) < 20 }

// isAddressShaped says whether s could be an address: bech32m is letters and digits, nothing else.
func isAddressShaped(s string) bool {
	if s == "" || len(s) > maxAddressLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') {
			return false
		}
	}
	return true
}

type homeData struct {
	Blocks   []BlockSummary
	Lanes    LaneStats
	Finality Finality
	Chart    Chart
}

func (s *Server) handleHome(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.Timeout)
	defer cancel()

	var d homeData
	var list BlockList
	// The ribbon of blocks carries more than a table — older ones are revealed by scrolling.
	if err := s.cli.Call(ctx, "getblocks", map[string]any{"count": 30}, &list); err != nil {
		s.fail(w, r, err)
		return
	}
	d.Blocks = list.Blocks
	// The lanes, finality and the chart are extras: when they fail, the page renders without them.
	_ = s.cli.Call(ctx, "getlanestats", nil, &d.Lanes)
	_ = s.cli.Call(ctx, "getfinality", nil, &d.Finality)
	rng := RangeByKey(r.URL.Query().Get("range"))
	var series HashrateSeries
	if err := s.cli.Call(ctx, "gethashrate",
		map[string]any{"blocks": rng.Blocks, "window": rng.Window, "points": rng.Points},
		&series); err == nil {
		d.Chart = BuildChart(series, rng)
	} else {
		d.Chart = Chart{Empty: true}
	}

	s.render(w, r, "home.html", "Network overview", "overview", d)
}

type blocksData struct {
	Blocks  []BlockSummary
	Tip     uint64
	Older   int64 // the height the next page continues from; -1 = there is nothing left
	Newer   int64
	PerPage int
}

func (s *Server) handleBlocks(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.Timeout)
	defer cancel()

	params := map[string]any{"count": s.cfg.BlocksPerPage}
	from := int64(-1)
	if v := r.URL.Query().Get("from"); v != "" {
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			s.fail(w, r, &Err{Msg: "Height must be a number."})
			return
		}
		from = int64(n)
		params["from"] = n
	}

	var list BlockList
	if err := s.cli.Call(ctx, "getblocks", params, &list); err != nil {
		s.fail(w, r, err)
		return
	}
	d := blocksData{Blocks: list.Blocks, Tip: list.Height, Older: -1, Newer: -1, PerPage: s.cfg.BlocksPerPage}
	if n := len(list.Blocks); n > 0 {
		if last := list.Blocks[n-1].Height; last > 0 {
			d.Older = int64(last) - 1
		}
		first := int64(list.Blocks[0].Height)
		if first < int64(list.Height) {
			d.Newer = min(first+int64(s.cfg.BlocksPerPage), int64(list.Height))
		}
	}
	if from < 0 {
		d.Newer = -1
	}
	s.render(w, r, "blocks.html", "Blocks", "blocks", d)
}

type blockData struct {
	Block Block
	// Txs are the transactions of the stretch [First, Last) of the block, in the block's order.
	Txs   []Tx
	First int
	Last  int
	// Prev and Next are the positions the neighbouring pages start at; -1 = there is none.
	Prev int
	Next int
}

func (s *Server) handleBlock(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.Timeout)
	defer cancel()

	id := r.PathValue("id")
	params := map[string]any{}
	// A number is a height, hex is a block id. The explorer tells them apart itself, so a visitor does
	// not have to know what belongs in the field.
	switch {
	case isHeight(id):
		h, _ := strconv.ParseUint(id, 10, 64)
		params["height"] = h
	case isHash(id):
		params["id"] = id
	default:
		s.fail(w, r, errShape)
		return
	}

	var b Block
	if err := s.cli.Call(ctx, "getblock", params, &b); err != nil {
		s.fail(w, r, err)
		return
	}
	d := blockData{Block: b, Prev: -1, Next: -1}
	// The transactions are loaded one by one, and each one is a call to the node. A full block has
	// hundreds of them, so the page shows one stretch of the block and links to the rest: what one
	// request can make the node do is bounded by the page, not by the block.
	//
	// A position that is not a number, or lies past the end of the block, is the first page.
	if n, err := strconv.Atoi(r.URL.Query().Get("txs")); err == nil && n > 0 && n < len(b.Txs) {
		d.First = n
	}
	end := min(d.First+blockTxLimit, len(b.Txs))
	for _, txid := range b.Txs[d.First:end] {
		var tx Tx
		if err := s.cli.Call(ctx, "getrawtransaction", map[string]any{"txid": txid}, &tx); err != nil {
			// Without the txindex transactions cannot be looked up; the block is shown anyway.
			break
		}
		d.Txs = append(d.Txs, tx)
	}
	d.Last = d.First + len(d.Txs)
	if len(d.Txs) > 0 {
		if d.First > 0 {
			d.Prev = max(d.First-blockTxLimit, 0)
		}
		if d.Last < len(b.Txs) {
			d.Next = d.Last
		}
	}
	s.render(w, r, "block.html", "Block "+strconv.FormatUint(b.Height, 10), "blocks", d)
}

// The caps on the lists of transactions. Each one is a call to the node, so without them a full
// block (96 kB of transactions, hundreds of them) would turn rendering the page into hundreds of calls.
const (
	recentTxBlocks = 10
	recentTxLimit  = 50
	blockTxLimit   = 50
)

type txsData struct {
	Txs    []Tx
	Blocks int
	// NoIndex means the blocks are there but the node cannot look transactions up by txid.
	// The page has to say so — "no transactions" would be untrue.
	NoIndex bool
}

// handleTxs lists the recent transactions from the recent blocks, from the newest.
func (s *Server) handleTxs(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.Timeout)
	defer cancel()

	var list BlockList
	if err := s.cli.Call(ctx, "getblocks", map[string]any{"count": recentTxBlocks}, &list); err != nil {
		s.fail(w, r, err)
		return
	}
	d := txsData{Blocks: len(list.Blocks)}
collect:
	for _, bs := range list.Blocks {
		var b Block
		if err := s.cli.Call(ctx, "getblock", map[string]any{"id": bs.ID}, &b); err != nil {
			continue
		}
		for _, txid := range b.Txs {
			if len(d.Txs) >= recentTxLimit {
				break collect
			}
			var tx Tx
			if err := s.cli.Call(ctx, "getrawtransaction", map[string]any{"txid": txid}, &tx); err != nil {
				// Without the txindex transactions cannot be looked up. It is not an outage (502),
				// but it is not "no transactions" either — the page says what is missing.
				d.NoIndex = true
				break collect
			}
			d.Txs = append(d.Txs, tx)
		}
	}
	s.render(w, r, "txs.html", "Transactions", "txs", d)
}

func (s *Server) handleTx(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.Timeout)
	defer cancel()

	txid := r.PathValue("txid")
	if !isHash(txid) {
		s.fail(w, r, errShape)
		return
	}
	var tx Tx
	if err := s.cli.Call(ctx, "getrawtransaction", map[string]any{"txid": txid}, &tx); err != nil {
		s.fail(w, r, err)
		return
	}
	s.render(w, r, "tx.html", "Transaction", "txs", tx)
}

// Address is a named field rather than an embedded struct: an embedded one is referred to in the
// template the same way as the field Address.Address, and `.Address` would print the whole struct
// instead of the address.
type addressData struct {
	Addr    Address
	Older   int64
	PerPage int
}

func (s *Server) handleAddress(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.Timeout)
	defer cancel()

	addr := r.PathValue("addr")
	if !isAddressShaped(addr) {
		s.fail(w, r, errShape)
		return
	}
	params := map[string]any{"address": addr, "limit": s.cfg.TxsPerPage}
	if v := r.URL.Query().Get("before"); v != "" {
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			s.fail(w, r, &Err{Msg: "before must be a number."})
			return
		}
		params["before"] = n
	}

	var a Address
	if err := s.cli.Call(ctx, "getaddress", params, &a); err != nil {
		s.fail(w, r, err)
		return
	}
	d := addressData{Addr: a, Older: -1, PerPage: s.cfg.TxsPerPage}
	// Paging goes by height, not by offset: with a new block an offset would shift the rows and one
	// would be lost.
	if n := len(a.Txs); n >= s.cfg.TxsPerPage && n > 0 {
		if h := a.Txs[n-1].Height; h > 0 {
			d.Older = int64(h)
		}
	}
	s.render(w, r, "address.html", "Address", "richlist", d)
}

func (s *Server) handleLanes(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.Timeout)
	defer cancel()

	var st LaneStats
	if err := s.cli.Call(ctx, "getlanestats", nil, &st); err != nil {
		s.fail(w, r, err)
		return
	}
	s.render(w, r, "lanes.html", "Algorithms", "algorithms", st)
}

func (s *Server) handleRichList(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.Timeout)
	defer cancel()

	var rl RichList
	if err := s.cli.Call(ctx, "getrichlist", map[string]any{"limit": 100}, &rl); err != nil {
		s.fail(w, r, err)
		return
	}
	s.render(w, r, "richlist.html", "Rich list", "richlist", rl)
}

// handleSearch decides from the shape of the input where it belongs and redirects. It renders nothing —
// that way the address in the browser stays one that can be passed on.
func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	b := s.cfg.BasePath
	switch {
	case q == "":
		http.Redirect(w, r, b+"/", http.StatusSeeOther)
	// A height is a number that fits into a uint64. A hash of nothing but digits is improbable, but a
	// 64-digit number is not a height and must not be treated as one.
	case isHeight(q):
		http.Redirect(w, r, b+"/block/"+q, http.StatusSeeOther)
	case isHash(q):
		// 64 hex characters is both a block id and a txid. The block is tried first, then the transaction —
		// guessing from nothing would mean sending half the searches to a 404.
		ctx, cancel := context.WithTimeout(r.Context(), s.cfg.Timeout)
		defer cancel()
		if err := s.cli.Call(ctx, "getblock", map[string]any{"id": q}, nil); err == nil {
			http.Redirect(w, r, b+"/block/"+q, http.StatusSeeOther)
			return
		}
		http.Redirect(w, r, b+"/tx/"+q, http.StatusSeeOther)
	default:
		http.Redirect(w, r, b+"/address/"+q, http.StatusSeeOther)
	}
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func isHex(s string) bool {
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}
