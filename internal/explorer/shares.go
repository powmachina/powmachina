package explorer

import (
	"context"
	"html/template"
	"net/http"
	"strings"

	"powmachina/internal/address"
	"powmachina/internal/identicon"
)

// Miners' icons and the share mosaic (the Sharepool spec): every miner has its icon and colour,
// drawn from its payout program, so a block's shares and the share pool read as a picture of who
// mines. Everything is SVG with its colours in attributes — the site's CSP allows no inline style.

// iconOf is the icon of an address: drawn from its program, so it is the same on every page and
// in the wallet. Anything that does not decode is drawn from its text.
func iconOf(addr string) identicon.Icon {
	if i := strings.LastIndexByte(addr, '1'); i > 0 {
		if _, prog, err := address.Decode(addr[:i], addr); err == nil {
			return identicon.For(prog)
		}
	}
	return identicon.For([]byte(addr))
}

// mosaicWidth is the mosaic's width in viewBox units.
const mosaicWidth = 1000.0

// Segment is one miner's part of a mosaic.
type Segment struct {
	X, W  float64
	Color string
	Title string
}

// Mosaic splits a bar between the miners by units, the most first; the miners the summary did not
// list come last, in grey. It takes a ShareTally or a pointer to one, as a template has either.
func Mosaic(v any) []Segment {
	var t *ShareTally
	switch x := v.(type) {
	case ShareTally:
		t = &x
	case *ShareTally:
		t = x
	}
	if t == nil || t.Units == 0 {
		return nil
	}
	var out []Segment
	x := 0.0
	var listed uint64
	for _, m := range t.Miners {
		w := float64(m.Units) / float64(t.Units) * mosaicWidth
		out = append(out, Segment{X: x, W: w, Color: iconOf(m.Address).Color(),
			Title: m.Address + " — " + plural(m.Shares, "share")})
		x += w
		listed += m.Units
	}
	if rest := t.Units - min(listed, t.Units); rest > 0 {
		out = append(out, Segment{X: x, W: float64(rest) / float64(t.Units) * mosaicWidth,
			Color: "hsl(0, 0%, 60%)", Title: plural(t.MinerCount-len(t.Miners), "more miner")})
	}
	return out
}

// iconHTML is the template's `icon`: a miner's icon, inline.
func iconHTML(addr string, px int) template.HTML {
	if addr == "" {
		return ""
	}
	return template.HTML(iconOf(addr).SVG(px)) // drawn from numbers alone, nothing of the input is in it
}

// unitShare is a miner's part of the units in per mille, for the table.
func unitShare(units, total uint64) int64 {
	if total == 0 {
		return 0
	}
	return int64(units * 1000 / total)
}

type sharesData struct {
	Pool   SharePool
	Mosaic []Segment
}

// handleShares shows the share pool: who has shares waiting for the next blocks.
func (s *Server) handleShares(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.Timeout)
	defer cancel()
	var p SharePool
	if err := s.cli.Call(ctx, "getsharepool", nil, &p); err != nil {
		s.fail(w, r, err)
		return
	}
	s.render(w, r, "shares.html", "Shares", "shares", sharesData{Pool: p, Mosaic: Mosaic(p.ShareTally)})
}
