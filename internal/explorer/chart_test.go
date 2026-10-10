package explorer

import (
	"fmt"
	"math"
	"strings"
	"testing"
)

func series(lane string, base int64, step int64, vals ...string) HashrateLane {
	l := HashrateLane{Lane: lane}
	for i, v := range vals {
		l.Points = append(l.Points, HashratePoint{T: base + int64(i)*step, HS: v})
	}
	return l
}

// The chart: two lanes with hashrates three orders of magnitude apart both have to fit into the image
// (each band has its own scale) and the last value goes both next to the band and into the legend.
func TestBuildChart(t *testing.T) {
	c := BuildChart(HashrateSeries{WindowBlocks: 12, Lanes: []HashrateLane{
		series("C", 1_800_000_000, 600, "12000", "15000", "14000"),
		series("G", 1_800_000_000, 600, "900000000", "1100000000", "1050000000"),
	}}, RangeByKey("6h"))
	if c.Empty || len(c.Lines) != 2 {
		t.Fatalf("the chart: empty=%v, %d bands", c.Empty, len(c.Lines))
	}
	// G is on top, C below it — both the drawing order and the order in the image.
	if c.Lines[0].Lane != "G" || c.Lines[1].Lane != "C" {
		t.Fatalf("the bands' order: %s, %s", c.Lines[0].Lane, c.Lines[1].Lane)
	}
	if c.Lines[0].Last != "1.0 GH/s" || c.Lines[1].Last != "14.0 kH/s" {
		t.Fatalf("the last values: %q, %q", c.Lines[0].Last, c.Lines[1].Last)
	}
	// Every band's whole outline lies inside the drawing area.
	for _, l := range c.Lines {
		for _, p := range strings.Fields(l.Band) {
			var x, y float64
			if _, err := fmt.Sscanf(p, "%f,%f", &x, &y); err != nil {
				t.Fatalf("point %q: %v", p, err)
			}
			if x < c.PadL-0.5 || x > c.W-c.PadR+0.5 || y < c.PadT-0.5 || y > c.H-c.PadB+0.5 {
				t.Fatalf("point %s is outside the chart area", p)
			}
		}
	}
	// The bands do not overlap and not even the weaker one disappears: lane C has a steady hashrate in
	// this series, and it still has to be at least bandMin of the area thick.
	_, gBot := bandEdges(t, c.Lines[0])
	cTop, cBot := bandEdges(t, c.Lines[1])
	if gBot >= cTop {
		t.Fatalf("the bands overlap: G ends at %.1f, C starts at %.1f", gBot, cTop)
	}
	plotH := c.H - c.PadT - c.PadB
	if cBot-cTop < bandMin*plotH-0.5 {
		t.Fatalf("lane C's band is %.1f thick, the minimum is %.1f", cBot-cTop, bandMin*plotH)
	}
	if !strings.Contains(c.Span, "20 min") {
		t.Fatalf("span %q", c.Span)
	}
}

// bandEdges returns a band's highest and lowest point.
func bandEdges(t *testing.T, l ChartLine) (float64, float64) {
	t.Helper()
	top, bot := math.Inf(1), math.Inf(-1)
	for _, p := range strings.Fields(l.Band) {
		var x, y float64
		if _, err := fmt.Sscanf(p, "%f,%f", &x, &y); err != nil {
			t.Fatalf("point %q: %v", p, err)
		}
		top, bot = math.Min(top, y), math.Max(bot, y)
	}
	return top, bot
}

// A short range has to have different labels on the x axis — otherwise three identical times stand there.
func TestChartXLabelsDiffer(t *testing.T) {
	c := BuildChart(HashrateSeries{WindowBlocks: 12, Lanes: []HashrateLane{
		series("G", 1_800_000_000, 20, "100", "120", "110", "130"),
	}}, RangeByKey("1h"))
	if c.XTicks[0].Label == c.XTicks[2].Label {
		t.Fatalf("identical x axis labels: %q", c.XTicks[0].Label)
	}
}

// One point is not a series — the chart is not drawn.
func TestChartNeedsTwoPoints(t *testing.T) {
	c := BuildChart(HashrateSeries{Lanes: []HashrateLane{series("G", 1_800_000_000, 60, "100")}},
		RangeByKey("6h"))
	if !c.Empty {
		t.Fatal("a chart out of one point")
	}
}

// The range switcher: the chart is trimmed to the chosen time, measured from the newest point. Without
// that one old block would stretch the axis over days and the last hour would be a strip at the edge.
func TestChartRangeTrimsOldPoints(t *testing.T) {
	// Points every 60 s over 3 hours.
	l := HashrateLane{Lane: "G"}
	base := int64(1_800_000_000)
	for i := range 181 { // exactly 3 h
		l.Points = append(l.Points, HashratePoint{T: base + int64(i)*60, HS: "1000"})
	}
	full := BuildChart(HashrateSeries{WindowBlocks: 12, Lanes: []HashrateLane{l}}, RangeByKey("6h"))
	hour := BuildChart(HashrateSeries{WindowBlocks: 12, Lanes: []HashrateLane{l}}, RangeByKey("1h"))
	if !strings.Contains(full.Span, "3 hours") {
		t.Fatalf("the whole range: %q", full.Span)
	}
	if !strings.Contains(hour.Span, "1 hour") {
		t.Fatalf("the 1H range: %q", hour.Span)
	}
	// The switcher knows the chosen range and offers the others as links.
	var active, links int
	for _, r := range hour.Ranges {
		if r.Active {
			active++
			if r.Key != "1h" {
				t.Fatalf("the marked range %q", r.Key)
			}
		}
		if r.Href == "?range="+r.Key {
			links++
		}
	}
	if active != 1 || links != len(ChartRanges) {
		t.Fatalf("the switcher: %d marked, %d links", active, links)
	}
	// An unknown range from the URL falls back to the default, not to an empty chart.
	if RangeByKey("../etc").Key != DefaultChartRange {
		t.Fatal("an unknown range did not fall back to the default")
	}
}
