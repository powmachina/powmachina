// powm-sharesim — a simulator of the Sharepool: a pool built into the protocol.
//
// It is a design tool, not consensus code. Miners publish shares — hashes under a target much
// easier than a block's — and every block pays the shares it includes. There is no pool operator:
// the reward is split by the chain. What it answers:
//
//   - rate     how often a small miner gets a share under a cap on the network's share rate, and
//     how much its daily reward varies (against a pool and against P2Pool)
//   - censor   whether a miner closing a block can gain by leaving everybody else's shares out,
//     under two reward rules — a block splitting its whole reward over what it includes, and a
//     fixed reward per unit of work with what is not paid going to a pot
//   - balance  whether a miner can keep a fork between two blocks unresolved by handing its votes
//     to the half of the network that is about to switch (the balance attack), under the rule
//     "more votes wins" and under "a lower block hash wins unless the other has a clear lead"
//   - payouts  how credits turn into coinbase outputs under the storage mass budget, how long a
//     payout waits, how much sits as credit — and what a pool that does not sign finality loses
//     when a part of the reward goes to the keys that sign
//
// The figures are for a 30 s block and 10 POWM per block (today's 40 POWM per 120 s rescaled).
//
//	go run ./cmd/powm-sharesim                    every scenario
//	go run ./cmd/powm-sharesim -scenario censor   one scenario
//	go run ./cmd/powm-sharesim -runs 20           more runs per cell
package main

import (
	"container/heap"
	"flag"
	"fmt"
	"math"
	"math/rand/v2"
	"sort"
)

const (
	blockTime   = 30.0 // seconds
	reward      = 10.0 // POWM per block
	day         = 86400.0
	cogs        = 1e8   // cogs per POWM (chain.POWM)
	massC       = 1e8   // storage mass constant in consensus (SPEC §1)
	cbMass      = 16000 // storage mass the template leaves for the coinbase
	cbOutputs   = 400   // outputs in one coinbase at most (bytes; ~40 B per output, a 16 kB slice)
	outputBytes = 43    // one payout output on the chain: value, script version, a 32 B program
)

// ---------------------------------------------------------------- rate

// rate is analytic: a device with 1/n of a lane's hashrate gets cap/2/n shares per second, and the
// number of shares in a day is Poisson — its relative spread is 1/sqrt(shares).
func scenarioRate(int) {
	fmt.Println("== rate — shares per day and the spread of the daily reward for a device holding 1/N of a lane")
	caps := []float64{10, 100, 300}
	fmt.Printf("   %-12s", "devices/lane")
	for _, c := range caps {
		fmt.Printf("  %22s", fmt.Sprintf("cap %g/s: shares · ±", c))
	}
	fmt.Printf("  %18s\n", "P2Pool 0.1/s · ±")
	for _, n := range []float64{100, 1e3, 1e4, 1e5, 1e6} {
		fmt.Printf("   %-12.0f", n)
		for _, c := range caps {
			s := c / 2 / n * day
			fmt.Printf("  %12.1f · %6.1f %%", s, spread(s))
		}
		s := 0.1 / n * day
		fmt.Printf("  %8.2f · %6.1f %%\n", s, spread(s))
	}
	fmt.Println("   (± is one standard deviation of a day's reward; a pool pays almost without spread,")
	fmt.Println("    but holds the miners' money and decides who mines what)")
	fmt.Printf("   raw shares on the chain at 14 B each: 10/s %.0f MB/day, 100/s %.0f MB/day, 300/s %.0f MB/day\n\n",
		10*14*day/1e6, 100*14*day/1e6, 300*14*day/1e6)
}

func spread(shares float64) float64 {
	if shares <= 0 {
		return math.Inf(1)
	}
	return 100 / math.Sqrt(shares)
}

// ---------------------------------------------------------------- censor

// A share in the censor model. Work is the same for every share — the miners use the minimum
// target; a heavier share is the same thing in fewer pieces.
type share struct {
	attacker bool
	born     float64 // when it was found
	seen     float64 // when the rest of the network has it
	parent   int     // the height of the block it builds on; it expires `expiry` blocks later
}

type censorCfg struct {
	alpha   float64 // the attacker's share of the hashrate
	latency float64 // mean propagation of a share or a block, seconds
	expiry  int     // a share can be included up to this many blocks above its parent
	rate    float64 // shares per second, the whole network
	censor  bool    // the attacker's blocks include only its own shares
	pot     bool    // the subsidy goes to a pot that pays a fixed price per share; otherwise a block splits its whole reward
}

type censorResult struct {
	attackerGain float64 // the attacker's reward / (alpha × all the reward paid)
	honestLost   float64 // honest shares that expired before any block took them, percent
	forks        float64 // blocks that lost a race, percent
}

// runCensor simulates `hours` of one lane-agnostic share stream. Blocks are closed by shares:
// each share closes one with probability 1/(rate·blockTime). Two blocks closer than the block
// latency race; an honest race is a coin toss, a race against the attacker's block goes the way
// the later shares vote, so the attacker wins it with probability alpha.
func runCensor(c censorCfg, hours float64, r *rand.Rand) censorResult {
	end := hours * 3600
	pClose := 1 / (c.rate * blockTime)

	var pending []share // found, not yet included in the active chain
	height := 0
	lastBlock := -1e9
	var paidAtt, paidAll, pot float64
	var lostHonest, honestTotal, blocks, forks int
	// The block on top, so that a block losing a race can be taken back.
	var last struct {
		att         bool
		take        []share
		pay, payAtt float64
		potBefore   float64
	}

	t := 0.0
	for t < end {
		t += r.ExpFloat64() / c.rate
		att := r.Float64() < c.alpha
		if !att {
			honestTotal++
		}
		// Every share is released at once; the attacker's censorship is in what its blocks take.
		pending = append(pending, share{attacker: att, born: t, seen: t + r.ExpFloat64()*c.latency, parent: height})

		if r.Float64() >= pClose {
			continue
		}
		// A block closed by this share. If the previous one is younger than the block latency,
		// the two race: between honest blocks a coin toss, against a censoring attacker's block
		// the later shares vote, so the attacker wins with probability alpha.
		if height > 0 && t-lastBlock < r.ExpFloat64()*c.latency {
			forks++
			pNew := 0.5
			if c.censor && att != last.att {
				if att {
					pNew = c.alpha
				} else {
					pNew = 1 - c.alpha
				}
			}
			if r.Float64() >= pNew {
				continue // this one lost; its closing share stays pending like any other
			}
			// The previous block lost: its shares go back, its payment is undone.
			paidAll -= last.pay
			paidAtt -= last.payAtt
			pot = last.potBefore
			pending = append(pending, last.take...)
			height--
			blocks--
		}
		blocks++
		lastBlock = t
		height++

		// What the closer includes: what it knows by now and has not expired; a censoring
		// attacker leaves out every honest share.
		var take, keep []share
		for _, p := range pending {
			switch {
			case height-p.parent > c.expiry:
				if !p.attacker {
					lostHonest++
				}
			case att && c.censor && !p.attacker:
				keep = append(keep, p)
			case p.seen <= t || (att && p.attacker):
				take = append(take, p)
			default:
				keep = append(keep, p)
			}
		}
		pending = keep
		last.att, last.take, last.potBefore = att, take, pot
		var pay, payAtt float64
		var na int
		for _, p := range take {
			if p.attacker {
				na++
			}
		}
		switch {
		case len(take) == 0 && !c.pot:
		case c.pot:
			// The rule as SHAREPOOL.md states it: every subsidy goes into the pot, and a block pays
			// each share it includes pot / (16 × the shares expected per block) — a price that does
			// not depend on how many shares the block took, so leaving shares out pays nothing.
			pot += reward
			n := float64(len(take))
			pay = math.Min(pot, pot*n*pClose/16)
			pot -= pay
		default:
			pay = reward
		}
		if len(take) > 0 {
			payAtt = pay * float64(na) / float64(len(take))
		}
		last.pay, last.payAtt = pay, payAtt
		paidAll += pay
		paidAtt += payAtt
	}
	res := censorResult{}
	if paidAll > 0 && c.alpha > 0 {
		res.attackerGain = paidAtt / paidAll / c.alpha
	}
	if honestTotal > 0 {
		res.honestLost = 100 * float64(lostHonest) / float64(honestTotal)
	}
	if blocks+forks > 0 {
		res.forks = 100 * float64(forks) / float64(blocks+forks)
	}
	return res
}

func scenarioCensor(runs int) {
	fmt.Println("== censor — a miner closing a block includes only its own shares")
	fmt.Println("   gain = the attacker's reward / its fair share (1.00 = fair); lost = honest shares that expired")
	fmt.Println("   rate 100 shares/s, 24 h per run, median of runs")
	type row struct {
		rule   string
		censor bool
		pot    bool
	}
	rows := []row{
		{"honest", false, false},
		{"censor, split", true, false},
		{"censor, pot", true, true},
	}
	for _, lat := range []float64{0.5, 2} {
		for _, exp := range []int{2, 8} {
			fmt.Printf("   latency %.1f s, expiry %d blocks\n", lat, exp)
			fmt.Printf("     %-16s", "rule")
			alphas := []float64{0.10, 0.25, 0.33, 0.45}
			for _, a := range alphas {
				fmt.Printf("  %18s", fmt.Sprintf("α=%.2f gain·lost", a))
			}
			fmt.Println()
			for _, rw := range rows {
				fmt.Printf("     %-16s", rw.rule)
				for _, a := range alphas {
					var g, l []float64
					for i := range runs {
						res := runCensor(censorCfg{alpha: a, latency: lat, expiry: exp, rate: 100,
							censor: rw.censor, pot: rw.pot}, 24, rand.New(rand.NewPCG(uint64(i), 0xc0ffee)))
						g = append(g, res.attackerGain)
						l = append(l, res.honestLost)
					}
					fmt.Printf("  %9.3f·%6.3f%%", median(g), median(l))
				}
				fmt.Println()
			}
		}
	}
	fmt.Println()
}

// ---------------------------------------------------------------- balance

// The balance model. A fork between block A and block B opens across a partition: the honest
// network is two halves, each sees its own half's shares after `inLat` and the other half's after
// `outLat`. Every share is a vote — it builds on the branch its miner prefers. The attacker is
// connected to both halves without delay and hands each of its shares to one half first (the other
// gets it `outLat` later, through the first half). The fork is resolved once both halves prefer the
// same branch and keep preferring it for a whole block time.

type delivery struct {
	at     float64
	group  int // 0 or 1: who learns about it
	branch int // 0 = A, 1 = B
}

type deliveries []delivery

func (d deliveries) Len() int           { return len(d) }
func (d deliveries) Less(i, j int) bool { return d[i].at < d[j].at }
func (d deliveries) Swap(i, j int)      { d[i], d[j] = d[j], d[i] }
func (d *deliveries) Push(x any)        { *d = append(*d, x.(delivery)) }
func (d *deliveries) Pop() any {
	old := *d
	x := old[len(old)-1]
	*d = old[:len(old)-1]
	return x
}

type balanceCfg struct {
	alpha  float64
	outLat float64 // seconds between the halves
	inLat  float64
	rate   float64 // shares per second, the whole network
	// margin: 0 = more votes wins (a tie stays with the block seen first); > 0 = the block with the
	// lower hash (A, by construction) wins unless B leads by more than `margin` shares.
	margin float64
}

// prefers is a half's choice given what it knows.
func (c balanceCfg) prefers(group int, votes [2]float64, knowsOther bool) int {
	if !knowsOther {
		return group // it has only seen its own block
	}
	if c.margin > 0 {
		if votes[1]-votes[0] > c.margin {
			return 1
		}
		return 0
	}
	switch {
	case votes[0] > votes[1]:
		return 0
	case votes[1] > votes[0]:
		return 1
	}
	return group
}

// runBalance returns the seconds until the fork resolved, or -1 when it had not within `limit`.
func runBalance(c balanceCfg, limit float64, r *rand.Rand) float64 {
	var view [2][2]float64 // view[group][branch]
	q := &deliveries{}
	knows := func(g int, t float64) bool { return t >= c.outLat } // the other half's block arrives after outLat
	agreeSince := -1.0
	t := 0.0
	for t < limit {
		t += r.ExpFloat64() / c.rate
		for q.Len() > 0 && (*q)[0].at <= t {
			d := heap.Pop(q).(delivery)
			view[d.group][d.branch]++
		}
		p0 := c.prefers(0, view[0], knows(0, t))
		p1 := c.prefers(1, view[1], knows(1, t))
		if p0 == p1 {
			if agreeSince < 0 {
				agreeSince = t
			}
			if t-agreeSince >= blockTime {
				return agreeSince
			}
		} else {
			agreeSince = -1
		}

		if r.Float64() < c.alpha {
			// The attacker: give the vote to the half closer to switching, for the branch it holds
			// now, so that it keeps holding it. Seen by that half at once, by the other later.
			g := 0
			if c.slack(1, view[1]) < c.slack(0, view[0]) {
				g = 1
			}
			b := c.prefers(g, view[g], knows(g, t))
			view[g][b]++
			heap.Push(q, delivery{at: t + c.outLat, group: 1 - g, branch: b})
			continue
		}
		g := r.IntN(2)
		b := g
		if g == 0 {
			b = p0
		} else {
			b = p1
		}
		heap.Push(q, delivery{at: t + c.inLat, group: g, branch: b})
		heap.Push(q, delivery{at: t + c.outLat, group: 1 - g, branch: b})
	}
	return -1
}

// slack is how many votes a half's current choice could absorb before it switches.
func (c balanceCfg) slack(group int, votes [2]float64) float64 {
	if c.margin > 0 {
		lead := votes[1] - votes[0]
		if lead > c.margin {
			return lead - c.margin
		}
		return c.margin - lead
	}
	return math.Abs(votes[0] - votes[1])
}

func scenarioBalance(runs int) {
	fmt.Println("== balance — a fork across a partition, the attacker feeds whichever half is about to switch")
	fmt.Println("   resolved = median seconds until both halves agree for a block time; open = runs unresolved after 1 h")
	fmt.Println("   rate 100 shares/s, latency inside a half 0.2 s")
	rules := []struct {
		name   string
		margin float64
	}{{"more votes", 0}, {"hash, margin 300", 300}, {"hash, margin 1000", 1000}}
	alphas := []float64{0, 0.10, 0.25, 0.33, 0.45}
	for _, out := range []float64{1, 3, 10} {
		fmt.Printf("   between halves %.0f s\n", out)
		fmt.Printf("     %-18s", "rule")
		for _, a := range alphas {
			fmt.Printf("  %16s", fmt.Sprintf("α=%.2f s·open", a))
		}
		fmt.Println()
		for _, rl := range rules {
			fmt.Printf("     %-18s", rl.name)
			for _, a := range alphas {
				var res []float64
				open := 0
				for i := range runs {
					v := runBalance(balanceCfg{alpha: a, outLat: out, inLat: 0.2, rate: 100, margin: rl.margin},
						3600, rand.New(rand.NewPCG(uint64(i), 0xba1)))
					if v < 0 {
						open++
						v = 3600
					}
					res = append(res, v)
				}
				fmt.Printf("  %9.0f·%2d/%-2d", median(res), open, runs)
			}
			fmt.Println()
		}
	}
	fmt.Println()
}

// ---------------------------------------------------------------- payouts

type key struct {
	frac      float64 // share of the network's work
	signs     bool
	credit    float64 // POWM
	eligible  float64 // when the credit last crossed the floor (-1 = below it)
	payouts   int
	firstPaid float64
	lastPaid  float64
}

type payoutCfg struct {
	keys     int
	floor    float64 // POWM; below it a credit waits
	every    float64 // seconds; a key is paid at most this often (0 = whenever it crosses the floor)
	signBon  float64 // part of the reward that goes to the keys that signed, 0..1
	silentAt float64 // the biggest key (a pool) does not sign
	days     float64
}

type payoutResult struct {
	payoutsPerDay    float64
	waitP50, waitP99 float64 // minutes from crossing the floor to the payout
	creditPOWM       float64 // credit outstanding at the end
	belowFloor       int     // keys whose credit is under the floor at the end
	smallFirstDays   float64 // the median key in the smallest decile: days to its first payout (-1 never)
	queueMax         int
	poolKeep         float64 // what the non-signing pool got / what it would get signing
}

// Hashrate is Pareto: a few pools and farms, a long tail of single devices.
func makeKeys(n int, r *rand.Rand) []*key {
	ks := make([]*key, n)
	var sum float64
	for i := range ks {
		w := math.Pow(1-r.Float64(), -1/1.1) // Pareto, shape 1.1
		ks[i] = &key{frac: w, signs: true, eligible: -1, firstPaid: -1, lastPaid: -1e18}
		sum += w
	}
	for _, k := range ks {
		k.frac /= sum
	}
	sort.Slice(ks, func(i, j int) bool { return ks[i].frac > ks[j].frac })
	return ks
}

// runPayouts: a settlement every 4 blocks (2 min, with the finality checkpoint). Each key's shares
// in it are Poisson around its part of the network's 100 shares/s, the settled reward is split by
// shares, part of it only among the keys that signed. Every block, the coinbase pays the credits
// above the floor, the oldest crossing first, as long as the storage mass and the output budget last.
func runPayouts(c payoutCfg, r *rand.Rand) payoutResult {
	ks := makeKeys(c.keys, r)
	if c.silentAt > 0 {
		ks[0].signs = false
	}
	const settleEvery = 4
	sharesPerSettle := 100 * blockTime * settleEvery
	var waits []float64
	var queue []*key
	var totalPayouts, queueMax int
	var poolGot, poolFair float64
	nBlocks := int(c.days * day / blockTime)
	for b := 1; b <= nBlocks; b++ {
		t := float64(b) * blockTime
		if b%settleEvery == 0 {
			pot := reward * settleEvery
			counts := make([]float64, len(ks))
			var total, signed float64
			for i, k := range ks {
				counts[i] = poisson(k.frac*sharesPerSettle, r)
				total += counts[i]
				if k.signs {
					signed += counts[i]
				}
			}
			if total > 0 {
				for i, k := range ks {
					got := pot * (1 - c.signBon) * counts[i] / total
					if k.signs && signed > 0 {
						got += pot * c.signBon * counts[i] / signed
					}
					if i == 0 {
						poolGot += got
						poolFair += pot * counts[i] / total
					}
					k.credit += got
					if k.eligible < 0 && k.credit >= c.floor && t-k.lastPaid >= c.every {
						k.eligible = t
						queue = append(queue, k) // crossings come in time order, so the queue stays sorted
					}
				}
			}
		}
		// The coinbase of block b: the oldest eligible first, as long as the budget lasts.
		if len(queue) > queueMax {
			queueMax = len(queue)
		}
		mass, n := 0.0, 0
		for n < len(queue) && n < cbOutputs {
			k := queue[n]
			m := massC / (k.credit * cogs)
			if mass+m > cbMass {
				break
			}
			mass += m
			waits = append(waits, (t-k.eligible)/60)
			if k.firstPaid < 0 {
				k.firstPaid = t
			}
			k.credit = 0
			k.eligible = -1
			k.lastPaid = t
			k.payouts++
			totalPayouts++
			n++
		}
		queue = queue[n:]
	}
	res := payoutResult{payoutsPerDay: float64(totalPayouts) / c.days, queueMax: queueMax}
	sort.Float64s(waits)
	if len(waits) > 0 {
		res.waitP50 = waits[len(waits)/2]
		res.waitP99 = waits[len(waits)*99/100]
	}
	var firsts []float64
	for i, k := range ks {
		res.creditPOWM += k.credit
		if i >= len(ks)*9/10 {
			if k.firstPaid < 0 {
				firsts = append(firsts, math.Inf(1))
			} else {
				firsts = append(firsts, k.firstPaid/day)
			}
		}
	}
	sort.Float64s(firsts)
	res.smallFirstDays = firsts[len(firsts)/2]
	if math.IsInf(res.smallFirstDays, 1) {
		res.smallFirstDays = -1
	}
	if poolFair > 0 {
		res.poolKeep = poolGot / poolFair
	}
	return res
}

func poisson(mean float64, r *rand.Rand) float64 {
	if mean > 50 {
		v := mean + math.Sqrt(mean)*r.NormFloat64()
		if v < 0 {
			return 0
		}
		return math.Round(v)
	}
	l, k, p := math.Exp(-mean), 0.0, 1.0
	for {
		p *= r.Float64()
		if p <= l {
			return k
		}
		k++
	}
}

func scenarioPayouts(runs int) {
	fmt.Println("== payouts — credits into coinbase outputs (storage mass 16,000, at most 400 outputs per block)")
	fmt.Println("   Pareto hashrate, settlement every 2 min, 7 days, median of runs")
	fmt.Printf("   %-7s %-7s %-6s %10s %8s %10s %10s %12s %14s\n",
		"keys", "floor", "every", "payouts/d", "MB/d", "wait p50", "wait p99", "credit POWM", "small 1st (d)")
	for _, n := range []int{1000, 10000, 100000} {
		for _, floor := range []float64{0.005, 0.05} {
			for _, every := range []float64{0, 6 * 3600, day} {
				var ppd, w50, w99, cr, sf []float64
				for i := range runs {
					res := runPayouts(payoutCfg{keys: n, floor: floor, every: every, days: 7},
						rand.New(rand.NewPCG(uint64(i), 0xfee)))
					ppd = append(ppd, res.payoutsPerDay)
					w50 = append(w50, res.waitP50)
					w99 = append(w99, res.waitP99)
					cr = append(cr, res.creditPOWM)
					sf = append(sf, res.smallFirstDays)
				}
				ev := "-"
				if every > 0 {
					ev = fmt.Sprintf("%gh", every/3600)
				}
				fmt.Printf("   %-7d %-7g %-6s %10.0f %8.1f %8.1f m %8.1f m %12.1f %14s\n", n, floor, ev,
					median(ppd), median(ppd)*outputBytes/1e6, median(w50), median(w99), median(cr), days(median(sf)))
			}
		}
	}
	fmt.Println("   (small 1st = days until the median key of the smallest tenth is first paid, \">7\" = not within the run)")

	fmt.Println("\n== signing — part of the reward only for the keys that signed the checkpoint")
	fmt.Println("   the biggest key (a pool) mines and does not sign; kept = what it gets / what it would get signing")
	fmt.Printf("   %-10s %10s %10s\n", "bonus", "pool size", "kept")
	for _, bon := range []float64{0.02, 0.05, 0.10} {
		res := runPayouts(payoutCfg{keys: 1000, floor: 0.05, days: 1, signBon: bon, silentAt: 1},
			rand.New(rand.NewPCG(1, 0x51)))
		ks := makeKeys(1000, rand.New(rand.NewPCG(1, 0x51)))
		fmt.Printf("   %-10s %9.1f%% %9.1f%%\n", fmt.Sprintf("%.0f %%", bon*100), 100*ks[0].frac, 100*res.poolKeep)
	}
	fmt.Println()
}

func days(d float64) string {
	if d < 0 {
		return ">7"
	}
	return fmt.Sprintf("%.2f", d)
}

func median(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	return s[len(s)/2]
}

func main() {
	only := flag.String("scenario", "", "run only this scenario: rate, censor, balance, payouts")
	runs := flag.Int("runs", 5, "runs per cell")
	flag.Parse()
	for _, sc := range []struct {
		name string
		run  func(int)
	}{{"rate", scenarioRate}, {"censor", scenarioCensor}, {"balance", scenarioBalance}, {"payouts", scenarioPayouts}} {
		if *only == "" || *only == sc.name {
			sc.run(*runs)
		}
	}
}
