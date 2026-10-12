// powmachina-wallet-rpc — wallets as a service for exchanges, pools and shops.
//
//	POWM_WALLET_PASSWORD_DEPOSIT=… POWM_WALLET_PASSWORD_HOT=… POWM_WALLET_RPC_TOKEN=… POWM_RPC_TOKEN=… \
//	  powmachina-wallet-rpc --wallet-file /data/deposit --wallet-file /data/hot \
//	      --change-address hot=powm1… --daemon-address powmachinad:7697
//
// Several wallets at once, like the bitcoind multiwallet: it is called as `/wallet/<name>`
// (the name is the file name). Wallets are created with powmachina-wallet-cli
// --generate-new-wallet. Passwords and tokens come from the environment, not from the
// arguments — otherwise they would sit in shell history and in the process list.
package main

import (
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

	"powmachina/internal/nodeclient"
	"powmachina/internal/wallet"
	"powmachina/internal/walletcmd"
	"powmachina/internal/walletrpc"
)

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func main() {
	var files, changes multiFlag
	flag.Var(&files, "wallet-file", "wallet file; may be repeated (the name for /wallet/<name> is the file name)")
	flag.Var(&changes, "change-address", "fixed change address of a wallet: name=address (the counterpart of HOT_MAIN_WALLET); may be repeated")
	testnet := flag.Bool("testnet", false, "testnet")
	regtest := flag.Bool("regtest", false, "regtest")
	daemon := flag.String("daemon-address", "", "node RPC host:port (default 127.0.0.1:<network RPC port>)")
	bind := flag.String("rpc-bind", "", "where to listen (default 127.0.0.1:<network wallet-rpc port>)")
	interval := flag.Duration("refresh", 10*time.Second, "how often to ask the node for new blocks")
	feeRate := flag.Int64("fee-per-kb", 0, "fee rate in cogs per 1 000 units of weight (0 = the network minimum)")
	noPass := flag.Bool("no-passphrase-required", false, "sign without walletpassphrase (development only — the RPC token alone then suffices to spend)")
	manage := flag.Bool("allow-manage", false, "let RPC create, restore and open wallets (for the GUI; an exchange must NOT have this)")
	flag.Parse()

	params, err := walletcmd.Network(*testnet, *regtest)
	if err != nil {
		log.Fatal(err)
	}
	// Without --allow-manage there has to be at least one wallet: otherwise the server would
	// have nothing to do. With it an empty start is fine — the GUI creates or opens a wallet itself.
	if len(files) == 0 && !*manage {
		log.Fatal("missing --wallet-file (create a wallet with powmachina-wallet-cli --generate-new-wallet, " +
			"or start with --allow-manage and create it over RPC)")
	}
	token := os.Getenv("POWM_WALLET_RPC_TOKEN")
	if token == "" {
		log.Fatal("missing POWM_WALLET_RPC_TOKEN — without a token wallet-rpc would sign a withdrawal for anybody who reaches the port")
	}
	node := nodeclient.New(walletcmd.DaemonAddress(params, *daemon), os.Getenv("POWM_RPC_TOKEN"))
	// A node on another network is refused at every sync: its blocks would be scanned as this network's.
	node.ExpectNetwork(params.Name)
	srv := walletrpc.New(node, node, token)
	srv.FeePerKB = *feeRate
	srv.RequirePassphrase = !*noPass
	srv.AllowManage = *manage
	srv.Params = params

	opened := map[string]*wallet.Wallet{}
	var order []string
	defer func() {
		for _, name := range order {
			if err := opened[name].Close(); err != nil {
				log.Printf("saving wallet %s: %v", name, err)
			}
		}
	}()
	for _, f := range files {
		name := filepath.Base(f)
		// Password: POWM_WALLET_PASSWORD_<NAME> (upper case, dashes as _), otherwise the shared
		// POWM_WALLET_PASSWORD, otherwise it is asked for.
		envName := "POWM_WALLET_PASSWORD_" + strings.ToUpper(strings.NewReplacer("-", "_", ".", "_").Replace(name))
		password := os.Getenv(envName)
		if password == "" {
			password = os.Getenv("POWM_WALLET_PASSWORD")
		}
		if password == "" {
			if password, err = walletcmd.ReadPassword(fmt.Sprintf("Password for wallet %s: ", name)); err != nil {
				log.Fatal(err)
			}
		}
		os.Unsetenv(envName)
		w, err := wallet.Open(f, password, params)
		if err != nil {
			log.Fatalf("wallet %s: %v", f, err)
		}
		if err := srv.AddWallet(name, w); err != nil {
			log.Fatal(err)
		}
		opened[name] = w
		order = append(order, name)
	}
	os.Unsetenv("POWM_WALLET_PASSWORD")
	for _, c := range changes {
		name, addr, ok := strings.Cut(c, "=")
		w := opened[name]
		if !ok || w == nil {
			log.Fatalf("--change-address %q: expected name=address for a loaded wallet", c)
		}
		if err := w.SetChangeAddress(addr); err != nil {
			log.Fatalf("--change-address %s: %v", name, err)
		}
		log.Printf("wallet %s: change always to %s", name, addr)
	}

	addr := *bind
	if addr == "" {
		addr = fmt.Sprintf("127.0.0.1:%d", params.WalletRPCPort)
	}
	httpSrv := srv.HTTPServer(addr)
	// We open the listener ourselves so the **real** address can be printed. With `127.0.0.1:0`
	// (how the GUI has a free port assigned to it) the log would otherwise say `:0` and the
	// caller would have no way to learn where to connect.
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("cannot listen on %s: %v", addr, err)
	}
	addr = ln.Addr().String()

	stop := make(chan struct{})
	for _, name := range order {
		go syncLoop(name, opened[name], node, *interval, stop)
	}
	// A wallet added while running (createwallet/loadwallet from the GUI) needs a loop of its
	// own — without it, it would pretend nothing ever landed on it.
	srv.OnWalletAdded = func(name string, w *wallet.Wallet) {
		log.Printf("wallet %s added while running — starting its sync", name)
		go syncLoop(name, w, node, *interval, stop)
	}
	go func() {
		log.Printf("wallet-rpc (%s) listening on %s, wallets: %s, walletpassphrase required: %v",
			params.Name, addr, strings.Join(order, ", "), srv.RequirePassphrase)
		if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("RPC: %v", err)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	close(stop)
	_ = httpSrv.Close()
	log.Printf("saving the wallets and shutting down")
}

func syncLoop(name string, w *wallet.Wallet, node wallet.Node, every time.Duration, stop <-chan struct{}) {
	for {
		res, err := w.Sync(node)
		switch {
		case errors.Is(err, wallet.ErrClosed):
			return // unloaded while running; a wallet loaded again gets a loop of its own
		case err != nil:
			log.Printf("%s: sync: %v", name, err)
		case res.To != res.From || len(res.Received) > 0 || res.RolledBack > 0:
			log.Printf("%s: height %d, new deposits %d, reorg %d blocks", name, res.To, len(res.Received), res.RolledBack)
		}
		select {
		case <-stop:
			return
		case <-time.After(every):
		}
	}
}
