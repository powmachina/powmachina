// powm-finsim — a simulator of the finality quorum made of the miners of the recent blocks.
//
// It is a design tool, not consensus code. A block carries its miner's finality key; the quorum
// for a height is every key in the window of blocks below it, weighted by the blocks it mined,
// each lane holding half of the weight. A block at the finality depth is final once keys holding
// more than the threshold of that weight have signed it.
//
// What it answers is how long finality stands still — after a big pool leaves, when its node stops
// signing, when somebody rents hashrate to mine and stay silent — for each window and threshold.
// Finality standing still is not the chain standing still: the chain keeps running under MESS.
//
//	go run ./cmd/powm-finsim                    every scenario, every candidate
//	go run ./cmd/powm-finsim -scenario offline  one scenario
//	go run ./cmd/powm-finsim -runs 200          more runs per cell
package main

import (
	"flag"
	"fmt"
	"math/rand/v2"
	"sort"
)

const (
	laneC = 0
	laneG = 1

	blockTime = 30.0 // chain.BlockTime; the lanes take turns
	depth     = 80   // chain.FinalityDepth
	hour      = 3600.0
	day       = 86400.0
	// stallBlocks: finality is said to stand still once it is this many blocks behind on top of
	// the depth — two blocks of jitter are not a stall.
	stallBlocks = 10
)

// miner is one key: a pool, a solo miner, an attacker.
type miner struct {
	name string
	// rate is the miner's hashrate on a lane at a time, relative to the honest network of that lane
	// at rest (1.0 = as much as everybody else together).
	rate func(lane int, t float64) float64
	// signs says whether the miner's node signs at a time.
	signs func(t float64) bool
}

type candidate struct {
	window    int
	threshold [2]int // numerator, denominator: more than num/den of the weight
	// active: a key without a block in this many blocks below the target has no seat (0 = off).
	active int
}

func (c candidate) String() string {
	s := fmt.Sprintf("W=%d T>%d/%d", c.window, c.threshold[0], c.threshold[1])
	if c.active > 0 {
		s += fmt.Sprintf(" A=%d", c.active)
	}
	return s
}

type scenario struct {
	name   string
	about  string
	length float64 // seconds simulated
	miners func() []miner
}

type result struct {
	stalledHours float64 // total time finality stood still
	longestHours float64 // the longest single stall
	firstStall   float64 // hours from the start of the simulation to the first stall (-1 = none)
}

func always(float64) bool { return true }

func flat(c, g float64) func(int, float64) float64 {
	return func(lane int, _ float64) float64 {
		if lane == laneC {
			return c
		}
		return g
	}
}

// between is a rate that holds only in [from, to).
func between(from, to float64, inner func(int, float64) float64) func(int, float64) float64 {
	return func(lane int, t float64) float64 {
		if t < from || t >= to {
			return 0
		}
		return inner(lane, t)
	}
}

// network is a typical young network: one big pool, a second one, and many solo miners.
func network(big, second float64, solos int) []miner {
	out := []miner{
		{name: "pool-a", rate: flat(big, big), signs: always},
		{name: "pool-b", rate: flat(second, second), signs: always},
	}
	rest := 1 - big - second
	for i := range solos {
		out = append(out, miner{name: fmt.Sprintf("solo-%d", i), rate: flat(rest/float64(solos), rest/float64(solos)), signs: always})
	}
	return out
}

var scenarios = []scenario{
	{
		name: "steady", about: "pool 60 %, pool 25 %, 15 solo miners; everybody signs",
		length: 5 * day,
		miners: func() []miner { return network(0.60, 0.25, 15) },
	},
	{
		name: "offline", about: "the 60 % pool goes offline for 6 h on day 2 (stops mining and signing)",
		length: 5 * day,
		miners: func() []miner {
			m := network(0.60, 0.25, 15)
			m[0].rate = func(lane int, t float64) float64 {
				if t >= 2*day && t < 2*day+6*hour {
					return 0
				}
				return 0.60
			}
			m[0].signs = func(t float64) bool { return t < 2*day || t >= 2*day+6*hour }
			return m
		},
	},
	{
		name: "gone", about: "the 60 % pool leaves for good on day 2",
		length: 5 * day,
		miners: func() []miner {
			m := network(0.60, 0.25, 15)
			m[0].rate = between(0, 2*day, flat(0.60, 0.60))
			m[0].signs = func(t float64) bool { return t < 2*day }
			return m
		},
	},
	{
		name: "mute", about: "the 60 % pool keeps mining, its node stops signing for 12 h on day 2",
		length: 5 * day,
		miners: func() []miner {
			m := network(0.60, 0.25, 15)
			m[0].signs = func(t float64) bool { return t < 2*day || t >= 2*day+12*hour }
			return m
		},
	},
	{
		name: "mute35", about: "a 35 % pool keeps mining, its node stops signing for 12 h on day 2",
		length: 5 * day,
		miners: func() []miner {
			m := network(0.35, 0.30, 15)
			m[0].signs = func(t float64) bool { return t < 2*day || t >= 2*day+12*hour }
			return m
		},
	},
	{
		name: "rent-g-2h", about: "an attacker rents 2x the lane G network for 2 h, mines in public, never signs",
		length: 5 * day,
		miners: func() []miner {
			return append(network(0.40, 0.30, 15), miner{name: "attacker",
				rate: between(2*day, 2*day+2*hour, flat(0, 2)), signs: func(float64) bool { return false }})
		},
	},
	{
		name: "rent-g-12h", about: "an attacker rents 2x the lane G network for 12 h, mines in public, never signs",
		length: 5 * day,
		miners: func() []miner {
			return append(network(0.40, 0.30, 15), miner{name: "attacker",
				rate: between(2*day, 2*day+12*hour, flat(0, 2)), signs: func(float64) bool { return false }})
		},
	},
	{
		name: "rent-both-2h", about: "an attacker rents 2x both lanes for 2 h, mines in public, never signs",
		length: 5 * day,
		miners: func() []miner {
			return append(network(0.40, 0.30, 15), miner{name: "attacker",
				rate: between(2*day, 2*day+2*hour, flat(2, 2)), signs: func(float64) bool { return false }})
		},
	},
	{
		name: "rent-both-12h", about: "an attacker rents 2x both lanes for 12 h, mines in public, never signs",
		length: 5 * day,
		miners: func() []miner {
			return append(network(0.40, 0.30, 15), miner{name: "attacker",
				rate: between(2*day, 2*day+12*hour, flat(2, 2)), signs: func(float64) bool { return false }})
		},
	},
}

var candidates = []candidate{
	{720, [2]int{1, 2}, 0}, {720, [2]int{2, 3}, 0},
	{1440, [2]int{1, 2}, 0}, {1440, [2]int{2, 3}, 0},
	{2880, [2]int{1, 2}, 0}, {2880, [2]int{2, 3}, 0},
	{5760, [2]int{1, 2}, 0}, {5760, [2]int{2, 3}, 0},
	{window: 2880, threshold: [2]int{2, 3}, active: 360}, {window: 2880, threshold: [2]int{2, 3}, active: 720},
}

// run simulates one chain: the lanes take turns, a block's miner is drawn by hashrate share in its
// lane at that moment, and the block at the depth below every new tip is signed by whoever signs.
func run(sc scenario, c candidate, rng *rand.Rand) result {
	miners := sc.miners()
	n := int(sc.length / blockTime)
	author := make([]int, n)
	res := result{firstStall: -1}

	lastFinal := -1
	stallStart := -1.0
	for h := range n {
		t := float64(h) * blockTime
		lane := h % 2
		var total float64
		for _, m := range miners {
			total += m.rate(lane, t)
		}
		pick := rng.Float64() * total
		author[h] = len(miners) - 1
		for i, m := range miners {
			if pick < m.rate(lane, t) {
				author[h] = i
				break
			}
			pick -= m.rate(lane, t)
		}

		// The block at the depth below this tip.
		target := h - depth
		if target >= 1 {
			from := max(0, target-c.window)
			var blocks [2]map[int]int
			blocks[laneC], blocks[laneG] = map[int]int{}, map[int]int{}
			var nLane [2]int
			for b := from; b < target; b++ {
				blocks[b%2][author[b]]++
				nLane[b%2]++
			}
			// Each lane holds half of the weight: a block of lane C weighs nG, a block of lane G nC.
			// Who mined in the last `active` blocks keeps a seat.
			recent := map[int]bool{}
			for b := max(from, target-c.active); c.active > 0 && b < target; b++ {
				recent[author[b]] = true
			}
			var signed, all int
			for i := range miners {
				if c.active > 0 && !recent[i] {
					continue
				}
				w := blocks[laneC][i]*nLane[laneG] + blocks[laneG][i]*nLane[laneC]
				all += w
				if miners[i].signs(t) {
					signed += w
				}
			}
			if all > 0 && signed*c.threshold[1] > all*c.threshold[0] {
				lastFinal = target
			}
		}

		behind := h - max(lastFinal, 0)
		stalled := lastFinal >= 0 && behind > depth+stallBlocks
		switch {
		case stalled && stallStart < 0:
			stallStart = t
			if res.firstStall < 0 {
				res.firstStall = t / hour
			}
		case !stalled && stallStart >= 0:
			d := (t - stallStart) / hour
			res.stalledHours += d
			res.longestHours = max(res.longestHours, d)
			stallStart = -1
		}
	}
	if stallStart >= 0 {
		d := (sc.length - stallStart) / hour
		res.stalledHours += d
		res.longestHours = max(res.longestHours, d)
	}
	return res
}

func median(xs []float64) float64 {
	sort.Float64s(xs)
	if len(xs) == 0 {
		return 0
	}
	return xs[len(xs)/2]
}

func main() {
	only := flag.String("scenario", "", "run only this scenario")
	runs := flag.Int("runs", 50, "runs per cell")
	flag.Parse()

	for _, sc := range scenarios {
		if *only != "" && sc.name != *only {
			continue
		}
		fmt.Printf("== %s — %s\n", sc.name, sc.about)
		fmt.Printf("   %-16s %14s %14s %14s\n", "candidate", "stalled h", "longest h", "stalls in")
		for _, c := range candidates {
			var stalled, longest []float64
			stalls := 0
			for i := range *runs {
				r := run(sc, c, rand.New(rand.NewPCG(uint64(i), 0x5eed)))
				stalled = append(stalled, r.stalledHours)
				longest = append(longest, r.longestHours)
				if r.firstStall >= 0 {
					stalls++
				}
			}
			fmt.Printf("   %-16s %14.1f %14.1f %10d/%d\n", c, median(stalled), median(longest), stalls, *runs)
		}
		fmt.Println()
	}
}
