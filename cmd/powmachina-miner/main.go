// powmachina-miner — the PowMachina miner: lane C on the CPU (RandomX v2) and lane G on the
// GPU (the CUDA plugin next to the binary) at the same time. Solo against your own node (RPC),
// or over stratum to a node with a stratum port or to a pool.
//
//	POWM_RPC_TOKEN=… powmachina-miner --address powm1… [--node 127.0.0.1:7697]
//	powmachina-miner --pool stratum+tcp://node-or-pool:7699 --address powm1… --worker rig1
//	powmachina-miner                  (no arguments: the saved settings, or the setup questions)
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"powmachina/internal/address"
	"powmachina/internal/chain"
	"powmachina/internal/gpu"
	"powmachina/internal/miner"
	"powmachina/internal/nodeclient"
	"powmachina/internal/stratum"
	"powmachina/internal/walletcmd"
)

// version is the miner version (set by the build: -ldflags "-X main.version=…").
var version = "dev"

func main() {
	addr := flag.String("address", "", "your PowMachina address for mining rewards (required)")
	node := flag.String("node", "", "solo: node RPC host:port (default 127.0.0.1:<network RPC port>)")
	pool := flag.String("pool", "", "stratum: stratum+tcp://host:port or stratum+ssl://host:port (a node with -stratum, or a pool)")
	worker := flag.String("worker", "", "stratum: name of this device (shown on the node/pool)")
	threads := flag.Int("threads", miner.DefaultThreads(), "CPU threads for lane C (default: cores, capped at L3 cache / 2 MiB)")
	noAff := flag.Bool("no-affinity", false, "do not pin CPU threads to cores")
	cpu := flag.Bool("cpu", true, "mine lane C on the CPU")
	gpuFlag := flag.String("gpu", "auto", "mine lane G on GPUs: auto (if the CUDA plugin and a card are present), on, off")
	gpuDevs := flag.String("gpu-devices", "", "GPU indexes to use, e.g. 0,2 (default: all)")
	cudaLib := flag.String("cuda-lib", "", "path to the GPU plugin, libpowm-cuda.so or powm-cuda.dll (default: next to the miner)")
	gpuDuty := flag.Int("gpu-duty", 100, "share of GPU time to use, in percent (20 = mine a fifth of the time, leave the rest to other work)")
	testnet := flag.Bool("testnet", false, "testnet")
	regtest := flag.Bool("regtest", false, "regtest")
	light := flag.Bool("light", false, "RandomX light mode (no 2 GiB dataset, much slower)")
	noHuge := flag.Bool("no-large-pages", false, "do not try large pages")
	bench := flag.Duration("benchmark", 0, "measure hashrate for this long (e.g. 60s) without a node, then exit")
	flag.Bool("setup", false, "on its own: ask for the settings again (the questions a first start with no arguments asks)")
	logger := log.New(os.Stdout, "", log.LstdFlags)
	// No arguments means a person, not a script: the saved settings, or the setup questions.
	flag.CommandLine.Parse(startupArgs(os.Args[1:], logger))
	defer hold()

	params, err := walletcmd.Network(*testnet, *regtest)
	if err != nil {
		die(logger, "%v", err)
	}
	if !*cpu {
		*threads = -1
	}
	gpus, names := openGPUs(*gpuFlag, *gpuDevs, *cudaLib, logger)

	if *bench > 0 {
		n, err := miner.Benchmark(miner.Options{Params: params, Threads: *threads, GPUs: gpus,
			Light: *light, LargePages: !*noHuge, NoAffinity: *noAff,
			GPUDuty: duty(*gpuDuty, logger), Log: logger}, *bench)
		if err != nil {
			die(logger, "%v", err)
		}
		for i, h := range n {
			if i == 0 && *threads < 0 {
				continue
			}
			rate := float64(h) / bench.Seconds()
			if i == 0 {
				logger.Printf("benchmark CPU (lane C, %d threads): %s", *threads, human(rate))
			} else {
				logger.Printf("benchmark GPU %d %s (lane G): %s", i-1, names[i-1], human(rate))
			}
		}
		return
	}

	if *addr == "" && *pool == "" {
		die(logger, "missing --address: where should the mining rewards go? (create one with powmachina-wallet-cli)")
	}
	var feed miner.Feed
	mode := ""
	if *pool != "" {
		login := *addr
		if *worker != "" {
			login += "." + *worker
		}
		feed = &stratum.Client{URL: *pool, Login: login, Pass: "x", Agent: "powmachina-miner/" + version, Log: logger}
		mode = "stratum " + *pool
	} else {
		v, prog, err := address.Decode(params.Bech32HRP, *addr)
		if err != nil || v != chain.ScriptVersionBasic || len(prog) != chain.PubkeyBytes {
			die(logger, "--address %s is not a valid %s address: %v", *addr, params.Name, err)
		}
		client := nodeclient.New(walletcmd.DaemonAddress(params, *node), rpcToken())
		netName, err := client.Network()
		if err != nil {
			die(logger, "cannot reach the node: %v", err)
		}
		if netName != params.Name {
			die(logger, "the node runs %s, but the miner was started for %s", netName, params.Name)
		}
		feed = &miner.NodeFeed{Src: client, Payout: prog, Params: params, Log: logger}
		mode = "solo " + walletcmd.DaemonAddress(params, *node)
	}

	m := miner.New(miner.Options{Params: params, Threads: *threads, GPUs: gpus,
		Light: *light, LargePages: !*noHuge, NoAffinity: *noAff,
		GPUDuty: duty(*gpuDuty, logger), Log: logger})
	what := []string{}
	if *threads > 0 {
		what = append(what, fmt.Sprintf("CPU %d threads (lane C)", *threads))
	}
	if len(gpus) > 0 {
		what = append(what, fmt.Sprintf("%d GPU (lane G)", len(gpus)))
	}
	logger.Printf("PowMachina miner %s — %s, %s, %s, rewards to %s", version, strings.Join(what, " + "), params.Name, mode, *addr)

	quit := make(chan struct{})
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		close(quit)
	}()
	go report(m, logger, quit, *threads > 0, names)
	if err := m.Run(feed, quit); err != nil {
		die(logger, "%v", err)
	}
	logger.Printf("stopped — accepted %d, rejected %d, blocks %d", m.Accepted.Load(), m.Rejected.Load(), m.Blocks.Load())
}

// report prints the speed every 30 s, per device.
func report(m *miner.Miner, logger *log.Logger, quit <-chan struct{}, cpu bool, gpuNames []string) {
	last := make([]uint64, len(m.DeviceHashes))
	lastT := time.Now()
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-quit:
			return
		case now := <-t.C:
			secs := now.Sub(lastT).Seconds() // for the printout only, not consensus
			var parts []string
			for i := range m.DeviceHashes {
				h := m.DeviceHashes[i].Load()
				rate := float64(h-last[i]) / secs
				last[i] = h
				switch {
				case i == 0 && cpu:
					parts = append(parts, "CPU "+human(rate))
				case i > 0:
					parts = append(parts, fmt.Sprintf("GPU%d %s", i-1, human(rate)))
				}
			}
			lastT = now
			logger.Printf("speed %s | accepted %d, rejected %d, blocks %d", strings.Join(parts, ", "),
				m.Accepted.Load(), m.Rejected.Load(), m.Blocks.Load())
		}
	}
}

// openGPUs loads the CUDA plugin and opens the cards. Without the plugin or a card only the CPU
// mines (with --gpu=on that is an error).
func openGPUs(mode, devs, lib string, logger *log.Logger) ([]miner.GPU, []string) {
	if mode == "off" {
		return nil, nil
	}
	pl, err := gpu.Load(lib)
	if err != nil {
		if mode == "on" {
			die(logger, "GPU: %v", err)
		}
		logger.Printf("GPU: CUDA plugin not found (%s) — CPU only", gpu.LibName)
		return nil, nil
	}
	all := pl.Devices()
	if len(all) == 0 {
		if mode == "on" {
			die(logger, "GPU: no NVIDIA card found")
		}
		logger.Printf("GPU: no NVIDIA card — CPU only")
		return nil, nil
	}
	want := map[int]bool{}
	for _, f := range strings.Split(devs, ",") {
		if f = strings.TrimSpace(f); f != "" {
			i, err := strconv.Atoi(f)
			if err != nil {
				die(logger, "--gpu-devices: %q is not a number", f)
			}
			want[i] = true
		}
	}
	var out []miner.GPU
	var names []string
	for _, d := range all {
		if len(want) > 0 && !want[d.Index] {
			continue
		}
		dev, err := pl.Open(d.Index)
		if err != nil {
			logger.Printf("GPU %d: %v", d.Index, err)
			continue
		}
		logger.Printf("GPU %d: %s, %.0f GB, sm_%s, %d SM, memory %.0f GB/s",
			len(out), d.Name, float64(d.Memory)/1e9, strings.ReplaceAll(d.SM, ".", ""), d.SMCount, d.PeakGBps)
		out = append(out, dev)
		names = append(names, d.Name)
	}
	return out, names
}

func human(hs float64) string {
	switch {
	case hs >= 1e6:
		return fmt.Sprintf("%.2f MH/s", hs/1e6)
	case hs >= 1e3:
		return fmt.Sprintf("%.2f kH/s", hs/1e3)
	}
	return fmt.Sprintf("%.0f H/s", hs)
}

// duty converts percent into a fraction and says what happened. 100 % = full speed (no waiting),
// less means the card is free for other work the rest of the time — the DAG stays in memory either way.
func duty(percent int, logger *log.Logger) float64 {
	if percent >= 100 || percent <= 0 {
		return 0
	}
	logger.Printf("GPU duty %d %%: between kernel launches the miner waits, so the card stays free the rest of the time", percent)
	return float64(percent) / 100
}
