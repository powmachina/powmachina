package explorer

import (
	"fmt"
	"math"
	"math/big"
	"strings"
	"time"
)

// The hashrate chart on the front page.
//
// It is drawn on the server as SVG — the explorer has neither JavaScript nor a charting library and we
// do not want to add them for one chart (the CSP is `script-src 'self'`, the page has to work without
// JS too).
//
// **The y axis is logarithmic.** Lane C is RandomX on processors (kH/s) and lane G runs on graphics
// cards (MH/s) — on a linear axis lane C would merge with zero. The difference between the lanes
// cannot be read as "who is stronger" anyway: they are two different algorithms and their hashrates
// are not comparable. The chart shows **how each of them develops**, which is why they are in one image.

// HashratePoint is one point from the node (`gethashrate`). The value is a string — the hashrate grows
// past 2⁵³ and a JSON number would round it.
type HashratePoint struct {
	T  int64  `json:"t"`
	HS string `json:"hs"`
	H  uint64 `json:"h"` // the block the window ends at — the chart links to it
}

// HashrateLane is one lane's series.
type HashrateLane struct {
	Lane   string          `json:"lane"`
	Points []HashratePoint `json:"points"`
}

// HashrateSeries is the answer of `gethashrate`.
type HashrateSeries struct {
	WindowBlocks int            `json:"window_blocks"`
	Lanes        []HashrateLane `json:"lanes"`
}

// ChartLine is one band of the chart (one lane). The band's thickness is that lane's hashrate, the
// edges are polylines — every point is one measurement and the chart should show how the hashrate
// really jumps.
type ChartLine struct {
	Lane           string
	Band           string  // the closed polygon of the band
	Edge           string  // the band's upper edge — a sharp line, the series is read on it
	Last           string  // the last value, formatted
	LabelX, LabelY float64 // the label inside the band near the right edge
}

// ChartRange is one time range of the switcher above the chart. It is switched with a link
// (`?range=6h`), not with JavaScript — the explorer has none and the CSP is `script-src 'self'`.
type ChartRange struct {
	Key    string        // "6h" — the value in the URL
	Label  string        // "6H" — what a person sees
	Span   time.Duration // how much time the chart shows
	Blocks int           // how many blocks to ask the node for
	Window int           // the sliding hashrate window
	Points int           // how many points to thin down to
	Active bool
	Href   string
}

// ChartRanges are the ranges offered. The block counts have margin for blocks arriving faster than
// the target 30 s (`blocks` is a cap, the trimming is done by Span).
var ChartRanges = []ChartRange{
	{Key: "1h", Label: "1H", Span: time.Hour, Blocks: 480, Window: 24, Points: 140},
	{Key: "6h", Label: "6H", Span: 6 * time.Hour, Blocks: 2800, Window: 48, Points: 180},
	{Key: "24h", Label: "24H", Span: 24 * time.Hour, Blocks: 10_400, Window: 48, Points: 220},
	{Key: "7d", Label: "7D", Span: 7 * 24 * time.Hour, Blocks: 72_000, Window: 96, Points: 260},
}

// DefaultChartRange is the range the page shows without a choice. A shorter range has denser points,
// so the curve shows how the hashrate jumps — over a week it would be a straight line.
const DefaultChartRange = "6h"

// RangeByKey finds the range by the value from the URL. An unknown value falls back to the default —
// the parameter comes from a visitor, so nothing other than what is in the list may be invented.
func RangeByKey(key string) ChartRange {
	for _, r := range ChartRanges {
		if r.Key == key {
			return r
		}
	}
	return RangeByKey(DefaultChartRange)
}

// ChartTip is one vertical strip of the chart: on hover (or tab) it shows the time, both lanes' values
// and the block the window ends at. The whole strip is a link to that block.
//
// No JavaScript: the strips are `<a>` elements in the SVG and the label is revealed through
// `:hover`/`:focus-within`.
type ChartTip struct {
	X, W   float64 // the area that reacts
	GuideX float64 // the vertical guide
	TipX   float64 // the label's left edge
	Anchor string  // "start" or "end" — near the right edge the label flips over
	Time   string
	Height uint64
	GPU    string // empty = the lane was not measuring at that time
	CPU    string
}

// ChartTick is a label on an axis.
type ChartTick struct {
	Pos    float64
	Label  string
	Anchor string // the text's alignment — the outermost x axis labels would otherwise stick out of the image
}

// Chart is the finished chart for the template. An empty Chart (Empty) is not drawn by the template.
type Chart struct {
	W, H         float64
	PadL, PadR   float64
	PadT, PadB   float64
	Lines        []ChartLine
	Tips         []ChartTip
	XTicks       []ChartTick
	WindowBlocks int
	Span         string // "last 6 hours"
	Ranges       []ChartRange
	Empty        bool
}

// The chart's dimensions per design 5 ("the chain ribbon"): wide and flat, the drawing area reaches
// almost to the edge — the values are in the legend below the chart, not next to the curves.
const (
	chartW    = 1180.0
	chartH    = 250.0
	chartPadL = 18.0
	chartPadR = 18.0
	chartPadT = 8.0
	chartPadB = 46.0 // the x axis labels

	// The band's thickness as a fraction of the area's height. The lower bound is there so a lane with a
	// steady hashrate stays visible — not so it looks as if it had fallen to zero.
	bandMin = 0.08
	bandMax = 0.42
	// The smallest range of the bands' scale, in decades: 0.3 is a factor of two. Without it, noise of a
	// few percent would stretch over the whole thickness and the chart would report drops that are not there.
	bandMinSpan = 0.3
	bandGap     = 9.0
	// The number of samples: both lanes are sampled at the same times, otherwise they cannot be stacked
	// on each other (every lane has its own blocks, hence its own times).
	bandSamples = 220
	// The number of strips that react to the mouse. On a 1144 px area that is ~16 px per strip —
	// fine enough to read and still a reasonably large target.
	chartTips = 72
	// The dimensions of the label at the cursor.
	tipW = 228.0
	tipH = 86.0
)

// BuildChart turns the node's answer into a finished chart for the chosen time range.
func BuildChart(s HashrateSeries, rng ChartRange) Chart {
	c := Chart{W: chartW, H: chartH, PadL: chartPadL, PadR: chartPadR, PadT: chartPadT,
		PadB: chartPadB, WindowBlocks: s.WindowBlocks, Empty: true, Ranges: rangeLinks(rng)}
	s = trimToRange(s, rng.Span)

	lanes := map[string][]lanePoint{}
	var minT, maxT int64
	minHS, maxHS := math.Inf(1), math.Inf(-1)
	for _, l := range s.Lanes {
		var pts []lanePoint
		for _, p := range l.Points {
			n, ok := new(big.Int).SetString(p.HS, 10)
			if !ok || n.Sign() <= 0 {
				continue
			}
			v, _ := new(big.Float).SetInt(n).Float64()
			pts = append(pts, lanePoint{p.T, v, p.H})
			if minT == 0 || p.T < minT {
				minT = p.T
			}
			if p.T > maxT {
				maxT = p.T
			}
			minHS = math.Min(minHS, v)
			maxHS = math.Max(maxHS, v)
		}
		if len(pts) > 1 {
			lanes[l.Lane] = pts
			c.Empty = false
		}
	}
	if c.Empty || maxT <= minT {
		c.Empty = true
		return c
	}

	x := func(t int64) float64 {
		return chartPadL + (chartW-chartPadL-chartPadR)*float64(t-minT)/float64(maxT-minT)
	}
	plotH := chartH - chartPadT - chartPadB

	// Every lane starts from its own lowest value: lane C is in kH/s, lane G in MH/s, on a shared axis C
	// would be left as a thread. The step is shared, though — one decade is as many pixels in either
	// band — so a lane that grew a hundredfold swells far more than one that doubled. Had each band
	// been stretched over its own range, the two would come out as mirror images whatever the lanes did.
	// The bands therefore **do not compare** the two lanes' sizes, only how much each one moved.
	type band struct {
		name   string
		xy     [][2]float64
		lo, hi float64
		thick  []float64
		last   float64
	}
	var bands []band
	// The step is the widest lane's range, and at least a factor of two.
	decades := bandMinSpan
	for _, name := range []string{"G", "C"} { // G on top, C below — as in the design
		pts := lanes[name]
		if len(pts) == 0 {
			continue
		}
		b := band{name: name, lo: math.Inf(1), hi: math.Inf(-1), xy: make([][2]float64, len(pts)),
			last: pts[len(pts)-1].hs}
		for i, p := range pts {
			b.lo = math.Min(b.lo, math.Log10(p.hs))
			b.hi = math.Max(b.hi, math.Log10(p.hs))
			b.xy[i] = [2]float64{float64(p.t), p.hs}
		}
		decades = math.Max(decades, b.hi-b.lo)
		bands = append(bands, b)
	}
	for bi := range bands {
		b := &bands[bi]
		// A lane that moved less than the widest one sits in the middle of the step, not at its bottom.
		lo := b.lo - (decades-(b.hi-b.lo))/2
		b.thick = make([]float64, bandSamples)
		for i := range bandSamples {
			t := minT + (maxT-minT)*int64(i)/int64(bandSamples-1)
			f := (math.Log10(sampleAt(b.xy, t)) - lo) / decades
			b.thick[i] = plotH * (bandMin + math.Min(math.Max(f, 0), 1)*(bandMax-bandMin))
		}
	}

	// The baseline floats: the whole column is centred, so growth of either lane spreads the band to both
	// sides and it does not look like a bar chart from the ground.
	tops := make([]float64, bandSamples)
	for i := range bandSamples {
		total := bandGap * float64(len(bands)-1)
		for _, b := range bands {
			total += b.thick[i]
		}
		tops[i] = chartPadT + (plotH-total)/2
	}

	for bi, b := range bands {
		top := make([][2]float64, bandSamples)
		bot := make([][2]float64, bandSamples)
		for i := range bandSamples {
			t := minT + (maxT-minT)*int64(i)/int64(bandSamples-1)
			y0 := tops[i]
			for k := 0; k < bi; k++ {
				y0 += bands[k].thick[i] + bandGap
			}
			top[i] = [2]float64{x(t), y0}
			bot[i] = [2]float64{x(t), y0 + b.thick[i]}
		}
		mid := (top[bandSamples-1][1] + bot[bandSamples-1][1]) / 2
		c.Lines = append(c.Lines, ChartLine{
			Lane: b.name, Band: closedBand(top, bot), Edge: points(top),
			Last: FormatHashrate(bigString(b.last)),
			// the label sits in the band near the right edge; the band is at least bandMin tall, so it fits
			LabelX: chartW - chartPadR - 8, LabelY: mid + 4,
		})
	}

	// The strips for the mouse: the time, both lanes and the block that can be clicked.
	c.Tips = buildTips(lanes, minT, maxT, x)

	// The x axis: the start, the middle and the end — more labels would overlap on a phone. The format
	// depends on the range: with a short one three identical "05:18" would say nothing, with a long one
	// the date is missing.
	span := maxT - minT
	layout := "15:04"
	switch {
	case span < 15*60:
		layout = "15:04:05"
	case span > 6*3600:
		// Over six hours the chart almost certainly crosses midnight — without the date "05:18 → 21:43"
		// would look like 16 hours backwards.
		layout = "01-02 15:04"
	}
	// Three labels: the start, the middle, the end — more would overlap on a phone. The outer ones are
	// aligned inwards, otherwise the text would end up outside the image and the browser would clip it.
	for i, t := range []int64{minT, (minT + maxT) / 2, maxT} {
		anchor := [3]string{"start", "middle", "end"}[i]
		c.XTicks = append(c.XTicks, ChartTick{Pos: x(t), Label: time.Unix(t, 0).UTC().Format(layout), Anchor: anchor})
	}
	c.Span = "last " + FormatDuration(time.Duration(maxT-minT)*time.Second)
	return c
}

// sampleAt returns a lane's value at any time. Between measurements it interpolates logarithmically
// (hashrate is a multiplicative quantity), outside the range it holds the outermost value — otherwise
// the band of a lane that started later would jump up from zero.
func sampleAt(pts [][2]float64, t int64) float64 {
	if len(pts) == 0 {
		return 1
	}
	ft := float64(t)
	if ft <= pts[0][0] {
		return pts[0][1]
	}
	if ft >= pts[len(pts)-1][0] {
		return pts[len(pts)-1][1]
	}
	for i := 1; i < len(pts); i++ {
		if pts[i][0] < ft {
			continue
		}
		a, b := pts[i-1], pts[i]
		w := (ft - a[0]) / (b[0] - a[0])
		return math.Exp(math.Log(a[1]) + w*(math.Log(b[1])-math.Log(a[1])))
	}
	return pts[len(pts)-1][1]
}

// points and closedBand: "x,y x,y …" for <polyline> and the closed outline of a band for <polygon>.
func points(p [][2]float64) string {
	var b strings.Builder
	for i, q := range p {
		if i > 0 {
			b.WriteByte(' ')
		}
		fmt.Fprintf(&b, "%.1f,%.1f", q[0], q[1])
	}
	return b.String()
}

func closedBand(top, bot [][2]float64) string {
	rev := make([][2]float64, len(bot))
	for i := range bot {
		rev[i] = bot[len(bot)-1-i]
	}
	return points(top) + " " + points(rev)
}

// lanePoint is one measurement of one lane prepared for drawing. The value is a float only for
// drawing — the hashrate is not computed anywhere else, it comes into the chart as a string.
type lanePoint struct {
	t  int64
	hs float64
	h  uint64
}

// buildTips splits the area into vertical strips and finds the **actual measured points** for each one
// (no interpolation — the label should hold a value that really occurred, not a computed one).
func buildTips(lanes map[string][]lanePoint, minT, maxT int64, x func(int64) float64) []ChartTip {
	span := maxT - minT
	if span <= 0 {
		return nil
	}
	nearest := func(pts []lanePoint, t int64) (float64, uint64, bool) {
		best, bestD := -1, int64(math.MaxInt64)
		for i, p := range pts {
			d := t - p.t
			if d < 0 {
				d = -d
			}
			if d < bestD {
				best, bestD = i, d
			}
		}
		if best < 0 {
			return 0, 0, false
		}
		// A point further away than two strips does not belong to this place — the lane was not measuring there.
		if bestD > span/chartTips*2+1 {
			return 0, 0, false
		}
		return pts[best].hs, pts[best].h, true
	}
	out := make([]ChartTip, 0, chartTips)
	w := (chartW - chartPadL - chartPadR) / chartTips
	for i := range chartTips {
		t := minT + span*int64(i)/int64(chartTips-1)
		tip := ChartTip{X: chartPadL + float64(i)*w, W: w, GuideX: x(t),
			Time: time.Unix(t, 0).UTC().Format("15:04:05")}
		if v, h, ok := nearest(lanes["G"], t); ok {
			tip.GPU = FormatHashrate(bigString(v))
			tip.Height = max(tip.Height, h)
		}
		if v, h, ok := nearest(lanes["C"], t); ok {
			tip.CPU = FormatHashrate(bigString(v))
			tip.Height = max(tip.Height, h)
		}
		if tip.GPU == "" && tip.CPU == "" {
			continue
		}
		tip.Anchor = "start"
		tip.TipX = tip.GuideX + 10
		if tip.TipX+tipW > chartW-chartPadR {
			tip.Anchor = "end"
			tip.TipX = tip.GuideX - 10 - tipW
		}
		out = append(out, tip)
	}
	return out
}

// rangeLinks assembles the switcher: every range with its own link, the chosen one marked.
func rangeLinks(active ChartRange) []ChartRange {
	out := make([]ChartRange, 0, len(ChartRanges))
	for _, r := range ChartRanges {
		r.Href = "?range=" + r.Key
		r.Active = r.Key == active.Key
		out = append(out, r)
	}
	return out
}

// trimToRange keeps only the points that fall into the chosen range. It is measured **from the newest
// point**, not from the clock: when the chain stands still for a while, the chart should show the last
// series rather than an empty area. The caption above the chart states the real range of the data
// anyway, not the range's name.
func trimToRange(s HashrateSeries, span time.Duration) HashrateSeries {
	var newest int64
	for _, l := range s.Lanes {
		for _, p := range l.Points {
			if p.T > newest {
				newest = p.T
			}
		}
	}
	if newest == 0 {
		return s
	}
	cutoff := newest - int64(span.Seconds())
	out := HashrateSeries{WindowBlocks: s.WindowBlocks}
	for _, l := range s.Lanes {
		kept := HashrateLane{Lane: l.Lane}
		for _, p := range l.Points {
			if p.T >= cutoff {
				kept.Points = append(kept.Points, p)
			}
		}
		out.Lanes = append(out.Lanes, kept)
	}
	return out
}

// bigString turns a value for drawing into an integer for FormatHashrate — formatting a hashrate has
// one place in the code and that one takes a string.
func bigString(v float64) string {
	if v <= 0 {
		return "0"
	}
	i, _ := big.NewFloat(v).Int(nil)
	return i.String()
}
