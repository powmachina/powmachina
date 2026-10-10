// mh-bench — MachinaHash calibration (the CPU side).
//
// Two lanes, two algorithms:
//
//   - lane C = MachinaHash-C (RandomX v2, PowMachina salt) — measured in fast mode, that is,
//     the way a miner runs.
//
//   - lane G = MachinaHash-G (B-core over the epoch DAG) — the CPU is deliberately weak here,
//     the number serves as the denominator for gate 2.
//
//     mh-bench [--dag-mb N] [--cache-mb N] [--threads N] [--epoch N] [--seconds N]
//     [--json PATH] [--skip-rx-fast] [--rx-insecure] [--no-large-pages]
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"powmachina/internal/mh"
)

func main() {
	var (
		dagMB       = flag.Int("dag-mb", 1024, "DAG size in MiB")
		cacheMB     = flag.Int("cache-mb", 16, "cache size in MiB")
		threads     = flag.Int("threads", runtime.NumCPU(), "number of threads")
		epoch       = flag.Uint64("epoch", 0, "epoch")
		seconds     = flag.Int("seconds", 4, "length of a single measurement in seconds")
		jsonPath    = flag.String("json", "", "also write the results as JSON into a file")
		skipRxFast  = flag.Bool("skip-rx-fast", false, "measure lane C in light mode (without the 2.1 GiB dataset)")
		rxInsecure  = flag.Bool("rx-insecure", false, "turn W^X off in the RandomX JIT — for measurement ONLY")
		noLargePage = flag.Bool("no-large-pages", false, "do not ask for large pages")
	)
	flag.Parse()

	fmt.Printf("# mh-bench — MachinaHash calibration (CPU)\n\n")
	fmt.Printf("CPU        : %s\n", cpuinfo("model name"))
	fmt.Printf("flags      : %s\n", cpuFlags())
	fmt.Printf("threads    : %d\n", *threads)
	fmt.Printf("DAG        : %d MiB (cache %d MiB, epoch %d)\n", *dagMB, *cacheMB, *epoch)
	fmt.Printf("RandomX    : %s\n", mh.RandomXFlags())
	if !mh.HasHardwareAES() {
		fmt.Println("  ⚠ without hardware AES — this will be orders of magnitude slower")
	}
	if !mh.JITIsSecure() {
		fmt.Println("  ⚠ JIT without W^X — a node like this should not process foreign blocks")
	}

	fmt.Println("\n## Preparation")
	params := mh.CustomDag(*epoch, *cacheMB<<20, *dagMB<<20)

	t := time.Now()
	cache := mh.BuildCache(params)
	fmt.Printf("DAG cache     : %9s\n", dur(time.Since(t)))

	t = time.Now()
	full := mh.BuildFullDagFrom(cache)
	dagMs := float64(time.Since(t).Microseconds()) / 1000
	fmt.Printf("DAG dataset   : %9s  (%d MiB)\n", dur(time.Since(t)), *dagMB)
	math := mh.NewRandMath(*epoch)

	t = time.Now()
	rx, err := mh.NewRandomXForMiner(*epoch, !*skipRxFast, !*rxInsecure, !*noLargePage)
	if err != nil {
		fmt.Fprintf(os.Stderr, "the RandomX context failed (fast mode needs ~2.1 GiB): %v\n", err)
		os.Exit(1)
	}
	defer rx.Close()
	rxMs := float64(time.Since(t).Microseconds()) / 1000
	rxMode := "fast"
	if *skipRxFast {
		rxMode = "light"
	}
	wx := "yes"
	if *rxInsecure {
		wx = "OFF (measurement only!)"
	}
	fmt.Printf("RandomX %-6s: %9s  (large pages: %s, W^X: %s)\n",
		rxMode, dur(time.Since(t)), yesNo(rx.UsesLargePages()), wx)

	fmt.Println("\n## Lanes")
	type row struct {
		Lane   string  `json:"lane"`
		HsOne  float64 `json:"hs_1t"`
		HsAll  float64 `json:"hs_all"`
		perOne float64 // ms/hash on a single thread
	}
	var rows []row

	// lane C — RandomX. The VM is built once per thread, not per hash.
	{
		one := measure(1, *seconds, func() func(uint64) {
			h, err := mh.NewHasherWithRandomX(rx)
			if err != nil {
				panic(err)
			}
			hdr := header(3)
			return func(n uint64) { _, _ = h.HashLaneC(hdr, n) }
		})
		all := measure(*threads, *seconds, func() func(uint64) {
			h, err := mh.NewHasherWithRandomX(rx)
			if err != nil {
				panic(err)
			}
			hdr := header(3)
			return func(n uint64) { _, _ = h.HashLaneC(hdr, n) }
		})
		fmt.Printf("### lane C (MachinaHash-C)  —  %.1f H/s (1 thread), %.1f H/s (%d threads), %.2f ms/hash\n",
			one, all, *threads, 1000/one)
		rows = append(rows, row{"C", one, all, 1000 / one})
	}

	// lane G — B-core
	{
		mk := func() func(uint64) {
			h := mh.NewHasher()
			hdr := header(5)
			return func(n uint64) { _, _ = h.HashWithDag(full, math, hdr, n, mh.LaneG) }
		}
		one := measure(1, *seconds, mk)
		all := measure(*threads, *seconds, mk)
		w := mh.LaneGWork()
		gbs := all * float64(w.DagBytes) / 1e9
		fmt.Printf("### lane G (MachinaHash-G)  —  %.1f H/s (1 thread), %.1f H/s (%d threads), %.2f ms/hash\n",
			one, all, *threads, 1000/one)
		fmt.Printf("    DAG %d KiB/hash → %.2f GB/s effective\n", w.DagBytes>>10, gbs)
		rows = append(rows, row{"G", one, all, 1000 / one})
	}

	fmt.Println("\n## Light verify (a node without the full DAG / without the RandomX dataset)")
	{
		light := mh.NewLightDagFrom(cache)
		h := mh.NewHasher()
		t := time.Now()
		if _, err := h.HashWithDag(light, math, header(7), 1, mh.LaneG); err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
		fmt.Printf("  lane G : %9s\n", dur(time.Since(t)))

		lightRx, err := mh.NewRandomX(*epoch, false)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  lane C : %v\n", err)
		} else {
			defer lightRx.Close()
			hc, err := mh.NewHasherWithRandomX(lightRx)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
			} else {
				defer hc.Close()
				t := time.Now()
				_, _ = hc.HashLaneC(header(7), 1)
				fmt.Printf("  lane C : %9s\n", dur(time.Since(t)))
			}
		}
		fmt.Println("  (the cost of verifying one block on a node that does not hold the full dataset)")
	}

	fmt.Println("\n## Gate (the algorithm spec §7)")
	cMs := rows[0].perOne
	ok := "BAD "
	if cMs >= 0.5 && cMs <= 2.0 {
		ok = "OK  "
	}
	fmt.Printf("  %s lane C within the 0.5–2 ms window : %.2f ms\n", ok, cMs)
	fmt.Println("  -- gate 1 (CPU/GPU for lane C) and gate 2 (GPU/CPU for lane G) need the GPU side;")
	fmt.Println("     compare with machinahash-cuda/README.md. The denominators from here:")
	fmt.Printf("     lane C %.0f H/s · lane G %.0f H/s\n", rows[0].HsAll, rows[1].HsAll)

	if *jsonPath != "" {
		out := map[string]any{
			"cpu": cpuinfo("model name"), "flags": cpuFlags(),
			"threads": *threads, "dag_mb": *dagMB, "cache_mb": *cacheMB, "epoch": *epoch,
			"dag_build_ms": round1(dagMs), "rx_build_ms": round1(rxMs), "rx_mode": rxMode,
			"hw_aes": mh.HasHardwareAES(), "jit_secure": mh.JITIsSecure(),
			"lanes": rows,
		}
		b, _ := json.Marshal(out)
		if err := os.WriteFile(*jsonPath, b, 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Printf("\nJSON → %s\n", *jsonPath)
	}
}

// measure measures the hashrate on `threads` threads. `mk` builds the work function for one
// thread — state that is built once (the RandomX VM, the mix buffer) therefore does not enter
// the measurement. A nonce never repeats, so nothing sits in a cache.
func measure(threads, seconds int, mk func() func(uint64)) float64 {
	var counter atomic.Uint64
	deadline := time.Now().Add(time.Duration(seconds) * time.Second)
	var wg sync.WaitGroup
	for t := range threads {
		wg.Add(1)
		go func(t int) {
			defer wg.Done()
			runtime.LockOSThread()
			f := mk()
			n := uint64(t) << 40
			var local uint64
			for time.Now().Before(deadline) {
				for range 8 {
					f(n)
					n++
					local++
				}
			}
			counter.Add(local)
		}(t)
	}
	wg.Wait()
	return float64(counter.Load()) / float64(seconds)
}

func header(seed byte) *[mh.HeaderBytes]byte {
	var h [mh.HeaderBytes]byte
	for i := range h {
		h[i] = byte(i)*seed + seed
	}
	return &h
}

func cpuinfo(key string) string {
	b, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return "?"
	}
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, key) {
			if _, v, ok := strings.Cut(l, ":"); ok {
				return strings.TrimSpace(v)
			}
		}
	}
	return "?"
}

func cpuFlags() string {
	want := []string{"avx512f", "avx2", "avx", "aes", "sse4_2", "bmi2", "vaes"}
	have := map[string]bool{}
	for _, f := range strings.Fields(cpuinfo("flags")) {
		have[f] = true
	}
	var out []string
	for _, w := range want {
		if have[w] {
			out = append(out, w)
		}
	}
	return strings.Join(out, ",")
}

func dur(d time.Duration) string { return d.Round(10 * time.Microsecond).String() }
func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "NO"
}
func round1(f float64) float64 { return float64(int64(f*10+0.5)) / 10 }
