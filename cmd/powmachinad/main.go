// powmachinad — the PowMachina node.
//
// Consensus parameters, the header, emission, difficulty and the lane rules; P2P, UTXO,
// mempool and RPC on top of them.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"powmachina/internal/address"
	"powmachina/internal/chain"
	"powmachina/internal/geoip"
	"powmachina/internal/mh"
	"powmachina/internal/p2p"
	"powmachina/internal/rpc"
	"powmachina/internal/store"
	"powmachina/internal/stratum"
)

func main() {
	selfcheck := flag.Bool("selfcheck", false, "verify that the algorithm and RandomX work")
	regtest := flag.Bool("regtest", false, "run on the regtest network")
	testnet := flag.Bool("testnet", false, "run on testnet")
	listenAddr := flag.String("listen", "", "address to listen on (e.g. 127.0.0.1:27696 on regtest; mainnet 7696)")
	connectAddr := flag.String("connect", "", "address of the peer to sync from")
	mine := flag.Int("mine", 0, "mine N blocks (regtest only, for development)")
	rpcAddr := flag.String("rpc", "", "address of the RPC server (e.g. 127.0.0.1:27697 on regtest; mainnet 7697); any other than loopback needs POWM_RPC_TOKEN")
	stratumAddr := flag.String("stratum", "", "stratum server for miners (e.g. 0.0.0.0:27699 on regtest; mainnet 7699)")
	stratumPay := flag.String("stratum-address", "", "pay every stratum block here (a single-owner farm); otherwise to the address from the miner login")
	mineAddr := flag.String("mineaddress", "", "address -mine pays to (without it a throwaway key, the reward is lost)")
	noFinality := flag.Bool("no-finality", false, "do not trust the finality quorum — run the chain on plain PoW (the finality spec §5)")
	nodeName := flag.String("nodename", "", "node label peers will see you under (max 24 characters)")
	txIndex := flag.Bool("txindex", false, "keep a transaction index (txid → block); the explorer and an exchange need it")
	addrIndex := flag.Bool("addrindex", false, "keep an address index and balances; the explorer needs it")
	anchors := flag.String("anchors", "", "file with anchor peers (restored after a restart)")
	addnode := flag.String("addnode", "", "peer addresses, comma separated (host:port)")
	allowPrivate := flag.Bool("allow-private", false, "allow connecting to private networks (10/8, 192.168/16, …) — for a closed deployment of your own; on the public network this looks like scanning and providers lock servers for it")
	datadir := flag.String("datadir", "", "directory with the chain; empty = memory only (resync after a restart)")
	blockSrc := flag.String("blocksource", "", "HTTP mirrors of block bodies, comma separated — a sync accelerator, not an authority (bodies are verified anyway)")
	serveBlocks := flag.String("serve-blocks", "", "address to mirror block bodies on over HTTP (opt-in)")
	serveBudget := flag.Int64("serve-budget-mib", 10240, "how many MiB the HTTP mirror may send (0 = no limit)")
	dag := flag.String("dag", "auto", "DAG for verifying lane G: auto (full while syncing when ~6 GiB of memory is available, light at the tip), full (~1 GiB+, miner), light (~26 MiB, slower first sync)")
	banList := flag.String("banlist", "", "file the node writes banned IPs into for the firewall (one IP or /64 per line)")
	geoipPath := flag.String("geoip", "", "DB-IP country CSV (.csv or .csv.gz) for the SharePool map of where shares come from; empty = off")
	geoipIgnore := flag.String("geoip-ignore", "", "addresses or networks of the network's own nodes, comma separated: shares they relay here are not counted on the map (they were counted where they came in)")
	metrics := flag.String("metrics", "", "address of the /metrics endpoint in Prometheus format (internal network only, e.g. 127.0.0.1:17694)")
	flag.Parse()

	// A typo in the name should show up right away, not once the node decides to start the network.
	if err := p2p.ValidateNodeName(*nodeName); err != nil {
		fmt.Fprintf(os.Stderr, "-nodename: %v\n", err)
		os.Exit(2)
	}

	fmt.Println("powmachinad — PowMachina (POWM)")
	printParams()

	if *selfcheck {
		if err := runSelfcheck(); err != nil {
			fmt.Fprintf(os.Stderr, "\nselfcheck FAILED: %v\n", err)
			os.Exit(1)
		}
		return
	}
	dagMode, err := parseDagMode(*dag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "-dag: %v\n", err)
		os.Exit(2)
	}
	if *mine > 0 && dagMode != dagFull {
		dagMode = dagFull // mining hashes millions of nonces — light would slow it a hundredfold
	}
	// Miners on the stratum port hand in shares, and only a node with the full DAG verifies them
	// (the Sharepool spec §7); in auto mode it would go light at the tip and refuse them all.
	if *stratumAddr != "" && dagMode != dagFull {
		dagMode = dagFull
		log.Printf("-stratum: the full DAG is on, the node verifies its miners' shares")
	}
	params, err := networkFlags(*testnet, *regtest)
	if err != nil {
		fmt.Fprintln(os.Stderr, "\n"+err.Error())
		os.Exit(2)
	}

	// --no-finality: the window is cleared before the node is assembled, so finality is off
	// everywhere at once (the node and the header chain) and there is no way to diverge.
	// Whoever does not trust the quorum runs plain PoW and is still on the same network
	// (the finality spec). Such a node neither counts the quorum nor holds a seat in it.
	if *noFinality {
		params.FinalityWindow = 0
		log.Printf("finality disabled by --no-finality: running plain PoW")
	}
	if *mine > 0 && params.Magic != chain.RegTest.Magic {
		// On testnet and mainnet mining is done by the miner, not the node: -mine is a
		// development shortcut that assembles blocks itself and bypasses stratum.
		fmt.Fprintln(os.Stderr, "\n-mine is for regtest only; elsewhere use powmachina-miner")
		os.Exit(2)
	}
	runNode(params, *listenAddr, *connectAddr, *rpcAddr, *stratumAddr, *stratumPay, *mineAddr, *anchors, *addnode, *datadir, *blockSrc, *serveBlocks, *metrics, *nodeName, *banList, dagMode, *serveBudget, *mine, *txIndex, *addrIndex, *allowPrivate, *geoipPath, *geoipIgnore)
}

// networkFlags picks the network. Mainnet cannot be picked until it has a genesis: better to
// refuse to start than to run on a placeholder pretending to be a real network's root of trust.
func networkFlags(testnet, regtest bool) (chain.NetParams, error) {
	switch {
	case testnet && regtest:
		return chain.NetParams{}, errors.New("-testnet and -regtest cannot be used together")
	case testnet:
		return chain.TestNet, nil
	case regtest:
		return chain.RegTest, nil
	}
	return chain.NetParams{}, errors.New("the mainnet genesis is not fixed yet — use -testnet or -regtest")
}

func runNode(params chain.NetParams, listenAddr, connectAddr, rpcAddr, stratumAddr, stratumPay, mineAddr, anchorsPath, addnode, datadir,
	blockSrc, serveBlocks, metricsAddr, nodeName, banList string, dag dagMode, serveBudgetMiB int64, mine int,
	txIndex, addrIndex, allowPrivate bool, geoipPath, geoipIgnore string) {
	genesis := chain.Genesis(params)
	if genesis == nil {
		log.Fatalf("network %s has no genesis", params.Name)
	}
	var node *Node
	if datadir == "" {
		log.Printf("⚠ without -datadir the chain lives in memory only — it is downloaded again after a restart")
		if txIndex || addrIndex {
			log.Printf("⚠ indexes make no sense without -datadir — they die with the chain")
		}
		node = NewNode(params, genesis, params.DagProfile)
	} else {
		dir := datadir
		// Regtest and mainnet must not share a directory — InitGenesis would refuse it, but
		// it is better not to hand it to them at all.
		dir = filepath.Join(dir, params.Name)
		var height uint64
		var err error
		node, height, err = OpenNodeWith(params, genesis, params.DagProfile, dir,
			store.Options{TxIndex: txIndex, AddrIndex: addrIndex})
		if err != nil {
			log.Fatalf("datadir %s: %v", dir, err)
		}
		log.Printf("datadir %s — continuing at height %d", dir, height)
	}

	if allowPrivate {
		// A closed deployment of your own (sentry nodes in a VPN, a testnet on the LAN). It
		// should not be on for the public network — see p2p/routable.go.
		node.AllowPrivateNet()
		log.Printf("⚠ -allow-private: connecting to private networks as well")
	}

	node.SetBanListPath(banList)
	avail, known := availableMemory()
	dag = dagModeFor(dag, avail, known)
	node.SetDagMode(dag)
	if geoipPath != "" {
		db, err := geoip.Load(geoipPath)
		if err != nil {
			log.Fatalf("-geoip: %v", err)
		}
		ignore, err := parsePrefixes(geoipIgnore)
		if err != nil {
			log.Fatalf("-geoip-ignore: %v", err)
		}
		node.SetShareGeo(db, ignore)
		log.Printf("geoip: %d ranges from %s — the SharePool map counts countries (%d own networks not counted)",
			db.Len(), geoipPath, len(ignore))
	}
	log.Printf("DAG: %s mode", dag)
	if err := node.SetNodeName(nodeName); err != nil {
		log.Fatalf("-nodename: %v", err)
	}
	if nodeName != "" {
		log.Printf("the node will introduce itself to peers as %q", nodeName)
	}

	if blockSrc != "" {
		for _, u := range strings.Split(blockSrc, ",") {
			if u = strings.TrimSpace(u); u != "" {
				node.blockSources = append(node.blockSources, u)
			}
		}
		log.Printf("HTTP body mirrors: %v (bodies are verified against the manifest anyway)", node.blockSources)
	}
	if serveBlocks != "" {
		go func() {
			if err := node.ServeBlocksHTTP(serveBlocks, serveBudgetMiB<<20); err != nil {
				log.Printf("⚠ the HTTP mirror stopped: %v", err)
			}
		}()
	}
	if metricsAddr != "" {
		go func() {
			if err := node.ServeMetricsHTTP(metricsAddr); err != nil {
				log.Printf("⚠ /metrics stopped: %v", err)
			}
		}()
		log.Printf("/metrics on %s — do not expose it to the internet (no token)", metricsAddr)
	}
	defer node.Close()
	log.Printf("network %s, genesis %x", params.Name, genesis.ID())
	if !node.chain.TrustedWork() {
		log.Printf("⚠ the chain has less work than the network expects (MinChainWork). Either it " +
			"is still catching up on history, or somebody handed you a cheap branch — while " +
			"this holds, `getinfo` reports trusted_work=false and the chain cannot be trusted.")
	}
	if key := node.FinalityKey(); key != nil {
		log.Printf("finality: a quorum of the miners of the last %d blocks, depth %d blocks; this node's key %x",
			params.FinalityWindow, chain.FinalityDepth, key[:])
	} else {
		log.Printf("finality: off, the chain runs on plain PoW")
	}

	if mineAddr != "" {
		if err := node.SetMinerAddress(mineAddr); err != nil {
			log.Fatalf("-mineaddress: %v", err)
		}
	} else if mine > 0 {
		log.Printf("⚠ -mine without -mineaddress: the reward goes to a throwaway key and is lost")
	}

	if mine > 0 {
		if err := node.MineBlocks(mine); err != nil {
			log.Fatalf("mining failed: %v", err)
		}
	}

	var ln net.Listener
	if listenAddr != "" {
		var err error
		ln, err = p2p.Listen(listenAddr)
		if err != nil {
			log.Fatalf("cannot listen on %s: %v", listenAddr, err)
		}
		log.Printf("listening on %s", ln.Addr())
		go node.Listen(ln)
	}

	if rpcAddr != "" {
		// The token comes from the environment so it ends up neither in shell history nor in the process list.
		token := os.Getenv("POWM_RPC_TOKEN")
		if err := checkRPCBind(rpcAddr, token); err != nil {
			log.Fatalf("RPC: %v", err)
		}
		srv := rpc.NewServer(node, token).HTTPServer(rpcAddr)
		// We open the listener ourselves so the **real** address can be printed. With
		// `127.0.0.1:0` (how the desktop wallet starts its node) the log would otherwise say
		// `:0` and the caller would have no way to learn where to connect.
		rpcLn, err := net.Listen("tcp", rpcAddr)
		if err != nil {
			log.Fatalf("RPC cannot listen on %s: %v", rpcAddr, err)
		}
		log.Printf("RPC listening on %s", rpcLn.Addr())
		go func() {
			if err := srv.Serve(rpcLn); err != nil && err != http.ErrServerClosed {
				log.Printf("RPC stopped: %v", err)
			}
		}()
	}

	if stratumAddr != "" {
		var payout []byte
		if stratumPay != "" {
			v, prog, err := address.Decode(params.Bech32HRP, stratumPay)
			if err != nil || v != chain.ScriptVersionBasic || len(prog) != chain.PubkeyBytes {
				log.Fatalf("-stratum-address: %s is not an address of network %s", stratumPay, params.Name)
			}
			payout = prog
		}
		sln, err := net.Listen("tcp", stratumAddr)
		if err != nil {
			log.Fatalf("stratum: cannot listen on %s: %v", stratumAddr, err)
		}
		srv := stratum.NewServer(node, stratum.ServerOptions{Payout: payout})
		node.SetStratum(srv)
		go func() {
			if err := srv.Serve(sln); err != nil {
				log.Printf("stratum stopped: %v", err)
			}
		}()
		who := "the address from the miner login"
		if payout != nil {
			who = stratumPay
		}
		log.Printf("stratum listening on %s, blocks pay %s", stratumAddr, who)
	}

	// Addresses first, connections second — otherwise the maintenance loop would have nobody to connect to.
	var manual []string
	if addnode != "" {
		manual = strings.Split(addnode, ",")
	}
	node.Bootstrap(context.Background(), manual)

	// Keeping connections up: filling outbound slots and a feeler against an eclipse attack.
	stop := make(chan struct{})
	node.StartNetwork(stop)
	node.ConnectAnchors(anchorsPath)

	if connectAddr != "" {
		if err := node.SyncFrom(connectAddr); err != nil {
			log.Fatalf("sync from %s failed: %v", connectAddr, err)
		}
	}

	// The node runs until somebody stops it. The one exception is `-mine N` — a development
	// shortcut that mines N blocks and exits.
	//
	// Waiting only when `ln != nil || rpcAddr != ""` would be wrong: `powmachinad -testnet`
	// — which is what everybody types the first time — would **exit silently** right after
	// startup: connect to the seeds, start its loops and fall out of the function. That
	// looks like a crash with no error message. The node takes care of the
	// network even without RPC and without `-listen` (it pulls the chain, holds connections,
	// collects bodies).
	if mine == 0 {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		<-sig
		log.Println("shutting down")
		if err := node.SaveAnchors(anchorsPath); err != nil {
			log.Printf("could not save the anchor peers: %v", err)
		}
		close(stop)
		if ln != nil {
			ln.Close()
		}
	}
}

func printParams() {
	fmt.Printf("\nConsensus\n")
	fmt.Printf("  block               %v (per lane %v)\n", chain.BlockTime, chain.LaneBlockTime)
	fmt.Printf("  max block           %d kB (coinbase up to %d kB)\n",
		chain.MaxBlockBytes/1024, chain.MaxCoinbaseBytes/1024)
	fmt.Printf("  header              %d B (PoW input %d B + nonce 8 B + lane 1 B)\n",
		chain.HeaderBytes, chain.PoWBytes)
	fmt.Printf("  lanes               C, G — at most %d of the same in a row, relief after %v\n",
		chain.MaxSameLaneRun, chain.LaneRotationRelief)
	fmt.Printf("  retarget            ASERT per lane, half-life %v, at most %d× per gap (LWMA, %d blocks, before the lane-difficulty fork)\n",
		chain.AsertHalfLife, 1<<chain.AsertMaxHalvings, chain.LwmaWindow)
	fmt.Printf("  DAG epoch           %d blocks\n", chain.EpochBlocks)

	fmt.Printf("\nEmission\n")
	fmt.Printf("  reward              %d POWM, halving every %d blocks\n",
		chain.InitialSubsidy/chain.POWM, chain.HalvingInterval)
	fmt.Printf("  slow start          %d blocks (%.1f → %d POWM)\n", chain.SlowStartBlocks,
		float64(chain.Subsidy(1))/float64(chain.POWM), chain.InitialSubsidy/chain.POWM)
	fmt.Printf("  dev fee             %.1f %% up to height %d\n",
		float64(chain.DevFeeBps)/100, chain.DevFeeEndHeight)
	fmt.Printf("  after halvings      ~%d M POWM, then %d.%03d POWM per block forever\n", chain.HalvingSupply/chain.POWM/1_000_000, chain.TailSubsidy/chain.POWM, chain.TailSubsidy%chain.POWM/100_000)

	fmt.Printf("\nAlgorithms\n")
	fmt.Printf("  lane C  MachinaHash-C — RandomX v2, PowMachina salt\n")
	fmt.Printf("  lane G  MachinaHash-G — B-core over the epoch DAG\n")
	fmt.Printf("  epoch   %d blocks\n", chain.EpochBlocks)
	fmt.Printf("  RandomX hw AES %v · JIT with W^X %v\n", mh.HasHardwareAES(), mh.JITIsSecure())
	if !mh.HasHardwareAES() {
		fmt.Printf("  ⚠ without hardware AES lane C is several times slower\n")
	}
	if !mh.JITIsSecure() {
		fmt.Printf("  ⚠ the JIT runs without W^X — a node like this should not process foreign blocks\n")
	}
}

func runSelfcheck() error {
	fmt.Printf("\nSelfcheck\n")
	start := time.Now()
	v := mh.NewVerifier(0, mh.TestProfile)
	defer v.Close()
	h := mh.NewHasher()
	defer h.Close()
	fmt.Printf("  DAG (test profile) built in %v\n", time.Since(start).Round(time.Millisecond))

	hdr := &chain.Header{Version: 1, Time: uint32(time.Now().Unix()), Bits: chain.RegtestTargetBits, Height: 1, Lane: mh.LaneG}
	for nonce := uint64(0); nonce < 20_000; nonce++ {
		hdr.Nonce = nonce
		ok, err := hdr.CheckPoW(h, v)
		if err != nil {
			return err
		}
		if ok {
			id := hdr.ID()
			fmt.Printf("  found nonce %d in lane %s\n", nonce, hdr.Lane)
			fmt.Printf("  block ID %x\n", id[:8])
			fmt.Printf("  the algorithm and the vendored RandomX both work\n")
			return nil
		}
	}
	return fmt.Errorf("no nonce found at target %x", hdr.Bits)
}
