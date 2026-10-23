// powm-diffsim — a simulator of the two-lane difficulty and the lane rotation.
//
// It is a design tool, not consensus code: the candidates are written in floating point here and
// the one that is chosen gets rewritten in integers in internal/chain. The LWMA below is checked
// against chain.NextBits by a test, so the baseline is the rule the network runs today.
//
//	go run ./cmd/powm-diffsim                 every scenario, every candidate
//	go run ./cmd/powm-diffsim -scenario drop  one scenario
//	go run ./cmd/powm-diffsim -runs 500       more runs per cell
package main

import (
	"flag"
	"fmt"
	"math"
	"math/rand/v2"
	"sort"
)

const (
	laneC = 0
	laneG = 1

	lwmaN    = 60  // chain.LwmaWindow
	mtpSpan  = 11  // chain.MedianTimeSpan
	baseRate = 1e6 // hashes per second of a lane at rest; only ratios matter
	hour     = 3600.0
	day      = 86400.0
)

// The lane's pace and the relief are the chain's unless a flag asks for others (`-spacing 240
// -relief 900` model a 120 s block, the Sharepool spec §8).
var (
	laneSpacing       = 60.0 // chain.LaneBlockTime
	relief      int64 = 300  // chain.LaneRotationRelief
)

// block is what the rules can see of a block (lane, t, target, relief) and what only the
// simulator knows (real).
type block struct {
	lane   int
	t      int64   // the header time
	real   float64 // when it was really found
	target float64 // probability that one hash is a block
	relief bool    // third of its lane in a row, let in by the relief
}

// rules is one candidate: how a block's time is bounded and how a lane's target is computed.
type rules struct {
	name string
	// strictTime: a block is not dated before its parent. Otherwise above the median of eleven.
	strictTime bool
	drift      float64
	next       func(s *sim, lane int) float64
}

type sim struct {
	r       rules
	rng     *rand.Rand
	blocks  []block
	byLane  [2][]int // indices into blocks
	nRelief [2]int
	rate    func(lane int, t float64) float64
	breaks  []float64 // times at which a rate changes
	floor   float64   // the easiest target there is
	// The attacker: a share of one lane that dates its blocks as early as the rules allow.
	attLane  int
	attShare float64
}

func eqTarget(rate float64) float64 { return 1 / (rate * laneSpacing) }

func newSim(r rules, seed uint64) *sim {
	s := &sim{r: r, rng: rand.New(rand.NewPCG(seed, 0x9e3779b97f4a7c15)), attLane: -1}
	s.floor = eqTarget(baseRate) * 1e6
	// A history at rest, so every window is full when the scenario starts at t = 0.
	n := 2 * (lwmaN + 10)
	for i := 0; i < n; i++ {
		t := float64(i-n+1) * laneSpacing / 2
		s.push(block{lane: i % 2, t: int64(t), real: t, target: eqTarget(baseRate)})
	}
	return s
}

func (s *sim) push(b block) {
	s.byLane[b.lane] = append(s.byLane[b.lane], len(s.blocks))
	if b.relief {
		s.nRelief[b.lane]++
	}
	s.blocks = append(s.blocks, b)
}

func (s *sim) tip() *block { return &s.blocks[len(s.blocks)-1] }

func (s *sim) blocked(lane int) bool {
	n := len(s.blocks)
	return s.blocks[n-1].lane == lane && s.blocks[n-2].lane == lane
}

// minTime is the earliest time the next block may carry.
func (s *sim) minTime() int64 {
	if s.r.strictTime {
		return s.tip().t
	}
	var ts [mtpSpan]int64
	for i := range ts {
		ts[i] = s.blocks[len(s.blocks)-1-i].t
	}
	sort.Slice(ts[:], func(i, j int) bool { return ts[i] < ts[j] })
	return ts[mtpSpan/2] + 1
}

// find returns when a lane finds a block if it starts hashing at `from`.
func (s *sim) find(lane int, from, target float64) float64 {
	t := from
	for {
		next := math.Inf(1)
		for _, b := range s.breaks {
			if b > t {
				next = b
				break
			}
		}
		if rate := s.rate(lane, t) * target; rate > 0 {
			if dt := s.rng.ExpFloat64() / rate; t+dt < next {
				return t + dt
			}
		}
		if math.IsInf(next, 1) {
			return next
		}
		t = next // hashing is memoryless, so the draw starts over at the new rate
	}
}

// step mines one block. It returns false when nobody can mine any more.
func (s *sim) step() bool {
	tip := s.tip()
	best, bestLane, bestTarget := math.Inf(1), -1, 0.0
	for lane := 0; lane < 2; lane++ {
		target := math.Min(s.r.next(s, lane), s.floor)
		from := tip.real
		if s.blocked(lane) {
			// The block has to be dated past the relief; an honest miner dates it with its clock.
			from = math.Max(from, float64(tip.t+relief+1))
		}
		if t := s.find(lane, from, target); t < best {
			best, bestLane, bestTarget = t, lane, target
		}
	}
	if bestLane < 0 {
		return false
	}
	b := block{lane: bestLane, real: best, target: bestTarget, relief: s.blocked(bestLane)}
	low := s.minTime()
	if b.relief && low < tip.t+relief+1 {
		low = tip.t + relief + 1
	}
	b.t = int64(best)
	if bestLane == s.attLane && s.rng.Float64() < s.attShare {
		b.t = low
	}
	if b.t < low {
		b.t = low
	}
	s.push(b)
	return true
}

func (s *sim) runUntil(t float64) {
	for s.tip().real < t && s.step() {
	}
}

// ---- the candidates ----

type lwmaOpts struct {
	monotonic    bool // a solvetime is measured from the highest time seen so far
	reliefCredit bool // the wait a relief block had no choice about is not its solvetime
	pendingGap   bool // the time since the lane's last block counts as a solvetime in progress
}

// lwma is chain.NextBits in floating point, with the options above.
func lwma(o lwmaOpts) func(s *sim, lane int) float64 {
	return func(s *sim, lane int) float64 {
		idx := s.byLane[lane]
		type entry struct {
			t      int64
			target float64
			relief bool
		}
		w := make([]entry, 0, lwmaN+2)
		for _, i := range idx[len(idx)-(lwmaN+1):] {
			b := &s.blocks[i]
			w = append(w, entry{b.t, b.target, b.relief})
		}
		if gap := s.tip().t - w[len(w)-1].t; o.pendingGap && float64(gap) > 6*laneSpacing {
			w = append(w[1:], entry{s.tip().t, w[len(w)-1].target, false})
		}
		var weighted, sum float64
		high := w[0].t
		for i := 1; i <= lwmaN; i++ {
			prev := w[i-1].t
			if o.monotonic {
				prev = high
			}
			st := float64(w[i].t - prev)
			if w[i].t > high {
				high = w[i].t
			}
			if o.reliefCredit && w[i].relief {
				st -= float64(relief)
			}
			st = math.Max(1, math.Min(st, 6*laneSpacing))
			weighted += st * float64(i)
			sum += w[i].target
		}
		avg := sum / lwmaN
		next := avg * weighted / (lwmaN * (lwmaN + 1) / 2 * laneSpacing)
		return math.Max(avg/4, math.Min(next, avg*4))
	}
}

// asert moves a lane's target by how far the lane is from the pace of one block per 240 s: the
// target doubles for every `halflife` it is behind. The clock is the tip's — the other lane's
// blocks move it too, so a lane that fell silent gets easier at the speed of real time, not at the
// speed of its own blocks.
//
// It is computed from the lane's last block: its target, and the time of the tip that block was
// built on. Without the floor that is the same number as counting from the genesis (`absolute`),
// but a lane that sat on the floor has its debt forgotten there, while the absolute form makes it
// mine every block it missed before its difficulty moves.
//
// `cap` bounds how many halvings one gap can ease the lane by (0 = no bound). Without it a lane
// that was gone for days comes back on the floor and stays nearly free for as long as it takes
// its own blocks to climb back, and those are held to two per block of the other lane.
func asert(halflife float64, reliefCredit, absolute bool, cap float64) func(s *sim, lane int) float64 {
	return func(s *sim, lane int) float64 {
		idx := s.byLane[lane]
		if absolute {
			behind := float64(s.tip().t-s.blocks[0].t) - laneSpacing*float64(len(idx))
			if reliefCredit {
				behind -= float64(relief) * float64(s.nRelief[lane])
			}
			return eqTarget(baseRate) * math.Pow(2, behind/halflife)
		}
		i := idx[len(idx)-1]
		last := &s.blocks[i]
		behind := float64(s.tip().t-s.blocks[i-1].t) - laneSpacing
		if reliefCredit && last.relief {
			behind -= float64(relief)
		}
		e := behind / halflife
		if cap > 0 {
			e = math.Max(-cap, math.Min(e, cap))
		}
		return last.target * math.Pow(2, e)
	}
}

func candidates() []rules {
	return []rules{
		{name: "today", drift: 120, next: lwma(lwmaOpts{})},
		{name: "lwma+time", strictTime: true, drift: 30, next: lwma(lwmaOpts{monotonic: true})},
		{name: "lwma+time+relief", strictTime: true, drift: 30, next: lwma(lwmaOpts{monotonic: true, reliefCredit: true})},
		{name: "lwma+time+relief+gap", strictTime: true, drift: 30, next: lwma(lwmaOpts{monotonic: true, reliefCredit: true, pendingGap: true})},
		{name: "asert-30m", strictTime: true, drift: 30, next: asert(0.5*hour, true, false, 0)},
		{name: "asert-1h", strictTime: true, drift: 30, next: asert(1*hour, true, false, 0)},
		{name: "asert-2h", strictTime: true, drift: 30, next: asert(2*hour, true, false, 0)},
		{name: "asert-4h", strictTime: true, drift: 30, next: asert(4*hour, true, false, 0)},
		{name: "asert-30m-cap4", strictTime: true, drift: 30, next: asert(0.5*hour, true, false, 4)},
		{name: "asert-1h-cap3", strictTime: true, drift: 30, next: asert(1*hour, true, false, 3)},
		{name: "asert-1h-cap4", strictTime: true, drift: 30, next: asert(1*hour, true, false, 4)},
		{name: "asert-1h-cap6", strictTime: true, drift: 30, next: asert(1*hour, true, false, 6)},
		{name: "asert-2h-cap4", strictTime: true, drift: 30, next: asert(2*hour, true, false, 4)},
		{name: "asert-1h-nocredit", strictTime: true, drift: 30, next: asert(1*hour, false, false, 0)},
		{name: "asert-1h-absolute", strictTime: true, drift: 30, next: asert(1*hour, true, true, 0)},
	}
}

// ---- the scenarios ----

func constant(c, g float64) func(int, float64) float64 {
	return func(lane int, _ float64) float64 {
		if lane == laneC {
			return c * baseRate
		}
		return g * baseRate
	}
}

// window counts what happened between two real times.
type window struct {
	blocks [2]float64
	relief float64
	logC   float64 // sum of ln(target / what the hashrate calls for), lane C
	logG   float64
	n      float64
}

func (s *sim) measure(from, to float64) window {
	var w window
	for i := range s.blocks {
		b := &s.blocks[i]
		if b.real < from || b.real >= to {
			continue
		}
		w.blocks[b.lane]++
		if b.relief {
			w.relief++
		}
	}
	return w
}

// ease is how many times easier a lane is right now than its hashrate calls for.
func (s *sim) ease(lane int, t float64) float64 {
	rate := s.rate(lane, t)
	if rate == 0 {
		rate = baseRate
	}
	return math.Min(s.r.next(s, lane), s.floor) / eqTarget(rate)
}

type result map[string]float64

type scenario struct {
	name, about string
	cols        []string
	run         func(r rules, seed uint64) result
}

func scenarios() []scenario {
	return []scenario{
		{
			name: "steady", about: "both lanes at rest for 5 days",
			cols: []string{"chain s/block", "C share %", "relief %", "C ease x", "C ease spread %"},
			run: func(r rules, seed uint64) result {
				s := newSim(r, seed)
				s.rate = constant(1, 1)
				var logs []float64
				for t := 6 * hour; t <= 5*day; t += hour {
					s.runUntil(t)
					logs = append(logs, math.Log(s.ease(laneC, t)))
				}
				w := s.measure(0, 5*day)
				n := w.blocks[0] + w.blocks[1]
				return result{"chain s/block": 5 * day / n, "C share %": 100 * w.blocks[0] / n,
					"relief %": 100 * w.relief / n, "C ease x": math.Exp(mean(logs)), "C ease spread %": 100 * stddev(logs)}
			},
		},
		drop("drop", 0.1),
		drop("drop99", 0.01),
		{
			name: "dead", about: "lane C is gone for 5 days",
			cols: []string{"chain s/block", "G ease at end x", "G ease max x"},
			run: func(r rules, seed uint64) result {
				s := newSim(r, seed)
				s.breaks = []float64{0}
				s.rate = func(lane int, t float64) float64 {
					if lane == laneC && t >= 0 {
						return 0
					}
					return baseRate
				}
				gMax := 0.0
				for t := hour; t <= 5*day; t += hour {
					s.runUntil(t)
					gMax = math.Max(gMax, s.ease(laneG, t))
				}
				w := s.measure(0, 5*day)
				return result{"chain s/block": 5 * day / (w.blocks[0] + w.blocks[1]),
					"G ease at end x": s.ease(laneG, 5*day), "G ease max x": gMax}
			},
		},
		{
			name: "revive", about: "lane C is gone for 5 days and then comes back whole",
			cols: []string{"C ease on return x", "C blocks 1st 6 h", "chain s/block 6 h", "C fair again h", "G ease max x"},
			run: func(r rules, seed uint64) result {
				s := newSim(r, seed)
				s.breaks = []float64{0, 5 * day}
				s.rate = func(lane int, t float64) float64 {
					if lane == laneC && t >= 0 && t < 5*day {
						return 0
					}
					return baseRate
				}
				s.runUntil(5*day - 1)
				onReturn := s.ease(laneC, 5*day)
				again, gMax := math.NaN(), 0.0
				for t := 5*day + 600; t <= 8*day; t += 600 {
					s.runUntil(t)
					if math.IsNaN(again) && s.ease(laneC, t) < 2 {
						again = (t - 5*day) / hour
					}
					gMax = math.Max(gMax, s.ease(laneG, t))
				}
				w := s.measure(5*day, 5*day+6*hour)
				return result{"C ease on return x": onReturn, "C blocks 1st 6 h": w.blocks[0],
					"chain s/block 6 h": 6 * hour / (w.blocks[0] + w.blocks[1]), "C fair again h": again, "G ease max x": gMax}
			},
		},
		{
			name: "backdate", about: "30 % of lane C dates every block as early as the rules allow, 5 days",
			cols: []string{"C s/block", "C ease mean x", "relief %"},
			run: func(r rules, seed uint64) result {
				s := newSim(r, seed)
				s.rate = constant(1, 1)
				s.attLane, s.attShare = laneC, 0.3
				var logs float64
				var n float64
				for t := 12 * hour; t <= 5*day; t += hour {
					s.runUntil(t)
					logs += math.Log(s.ease(laneC, t))
					n++
				}
				w := s.measure(12*hour, 5*day)
				return result{"C s/block": 4.5 * day / w.blocks[0], "C ease mean x": math.Exp(logs / n),
					"relief %": 100 * w.relief / (w.blocks[0] + w.blocks[1])}
			},
		},
		{
			name: "hop", about: "lane C is 1x for 2 h, then 5x for 2 h, over and over for 5 days",
			cols: []string{"C s/block at 1x", "C s/block at 5x", "hopper gain x"},
			run: func(r rules, seed uint64) result {
				s := newSim(r, seed)
				for t := 0.0; t <= 5*day; t += 2 * hour {
					s.breaks = append(s.breaks, t)
				}
				high := func(t float64) bool { return t >= 0 && int(t/(2*hour))%2 == 1 }
				s.rate = func(lane int, t float64) float64 {
					if lane == laneC && high(t) {
						return 5 * baseRate
					}
					return baseRate
				}
				s.runUntil(5 * day)
				var lo, hi float64
				for _, i := range s.byLane[laneC] {
					if b := &s.blocks[i]; b.real >= 12*hour && b.real < 5*day {
						if high(b.real) {
							hi++
						} else {
							lo++
						}
					}
				}
				span := 4.5 * day / 2
				// Blocks per hash of the 4x that comes and goes, against the 1x that stays.
				gain := (hi * 4 / 5 / 4) / (lo + hi/5)
				return result{"C s/block at 1x": span / lo, "C s/block at 5x": span / hi, "hopper gain x": gain * 2}
			},
		},
	}
}

// drop is lane C losing most of its hashrate and getting it back.
func drop(name string, left float64) scenario {
	return scenario{
		name: name, about: fmt.Sprintf("lane C falls to %g %% of its hashrate for 2 days, then gets it back", 100*left),
		cols: []string{"blocks first 6 h", "C fair after h", "G ease max x", "C blocks 1st h back", "C fair again h"},
		run: func(r rules, seed uint64) result {
			s := newSim(r, seed)
			s.breaks = []float64{0, 2 * day}
			s.rate = func(lane int, t float64) float64 {
				if lane == laneC && t >= 0 && t < 2*day {
					return left * baseRate
				}
				return baseRate
			}
			fair, gMax := math.NaN(), 0.0
			for t := 600.0; t <= 2*day; t += 600 {
				s.runUntil(t)
				if math.IsNaN(fair) && s.ease(laneC, t-1) > 0.5 {
					fair = t / hour
				}
				gMax = math.Max(gMax, s.ease(laneG, t-1))
			}
			again := math.NaN()
			for t := 2*day + 600; t <= 4*day; t += 600 {
				s.runUntil(t)
				if math.IsNaN(again) && s.ease(laneC, t) < 2 {
					again = (t - 2*day) / hour
				}
			}
			first := s.measure(0, 6*hour)
			back := s.measure(2*day, 2*day+hour)
			return result{"blocks first 6 h": first.blocks[0] + first.blocks[1], "C fair after h": fair,
				"G ease max x": gMax, "C blocks 1st h back": back.blocks[0], "C fair again h": again}
		},
	}
}

func mean(v []float64) float64 {
	var m float64
	for _, x := range v {
		m += x
	}
	return m / float64(len(v))
}

func stddev(v []float64) float64 {
	m, q := mean(v), 0.0
	for _, x := range v {
		q += (x - m) * (x - m)
	}
	return math.Sqrt(q / float64(len(v)))
}

func main() {
	runs := flag.Int("runs", 200, "runs per candidate and scenario")
	only := flag.String("scenario", "", "run one scenario (steady, drop, drop99, dead, revive, backdate, hop)")
	flag.Float64Var(&laneSpacing, "spacing", laneSpacing, "seconds per block of one lane")
	flag.Int64Var(&relief, "relief", relief, "seconds after which the rotation rule is waived")
	flag.Parse()

	for _, sc := range scenarios() {
		if *only != "" && *only != sc.name {
			continue
		}
		fmt.Printf("\n%s — %s (median of %d runs, %g s lane, relief %d s)\n", sc.name, sc.about, *runs, laneSpacing, relief)
		fmt.Printf("  %-22s", "")
		for _, c := range sc.cols {
			fmt.Printf(" %20s", c)
		}
		fmt.Println()
		for _, r := range candidates() {
			got := map[string][]float64{}
			for i := 0; i < *runs; i++ {
				for k, v := range sc.run(r, uint64(i)+1) {
					got[k] = append(got[k], v)
				}
			}
			fmt.Printf("  %-22s", r.name)
			for _, c := range sc.cols {
				fmt.Printf(" %20s", median(got[c]))
			}
			fmt.Println()
		}
	}
}

// median prints the middle value; a run in which the thing never happened counts as "never".
func median(v []float64) string {
	sort.Slice(v, func(i, j int) bool {
		if math.IsNaN(v[i]) {
			return false
		}
		return math.IsNaN(v[j]) || v[i] < v[j]
	})
	m := v[len(v)/2]
	if math.IsNaN(m) {
		return "never"
	}
	return fmt.Sprintf("%.2f", m)
}
