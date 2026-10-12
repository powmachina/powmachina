// powmachina-wallet-cli — a wallet for people, in the style of monero-wallet-cli.
//
//	powmachina-wallet-cli                                  (asks for the file name)
//	powmachina-wallet-cli --generate-new-wallet mywallet
//	powmachina-wallet-cli --generate-new-wallet mywallet --restore-from-seed
//	powmachina-wallet-cli --wallet-file mywallet
//	powmachina-wallet-cli --wallet-file mywallet --command balance
//
// One file, encrypted with a password (Argon2id + XChaCha20-Poly1305). The wallet connects to a
// node (--daemon-address) and scans the blocks itself — the node does not know which addresses
// are yours.
package main

import (
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"powmachina/internal/bip39"
	"powmachina/internal/chain"
	"powmachina/internal/nodeclient"
	"powmachina/internal/wallet"
	"powmachina/internal/walletcmd"
)

type cli struct {
	w      *wallet.Wallet
	node   *nodeclient.Client
	params chain.NetParams
	out    sync.Mutex // output from the background does not mix with a command's output
}

func main() {
	gen := flag.String("generate-new-wallet", "", "create a new wallet file")
	file := flag.String("wallet-file", "", "open an existing wallet file")
	restore := flag.Bool("restore-from-seed", false, "with --generate-new-wallet: restore from 24 words")
	restoreHeight := flag.Uint64("restore-height", 0, "with --restore-from-seed: scan from this block height")
	testnet := flag.Bool("testnet", false, "use testnet")
	regtest := flag.Bool("regtest", false, "use regtest")
	daemon := flag.String("daemon-address", "", "node RPC host:port (default 127.0.0.1:<network RPC port>)")
	command := flag.String("command", "", "run one command and exit (e.g. \"balance\")")
	flag.Parse()

	params, err := walletcmd.Network(*testnet, *regtest)
	if err != nil {
		fatal(err)
	}
	c := &cli{params: params, node: nodeclient.New(walletcmd.DaemonAddress(params, *daemon), os.Getenv("POWM_RPC_TOKEN"))}

	// A node on another network is refused at every refresh: its blocks would be scanned as this network's.
	c.node.ExpectNetwork(params.Name)

	fmt.Printf("PowMachina wallet (%s)\n", params.Name)
	switch {
	case *gen != "":
		c.create(*gen, *restore, *restoreHeight)
	case *file != "":
		c.open(*file)
	default:
		c.ask()
	}
	defer func() {
		if err := c.w.Close(); err != nil {
			fmt.Println("Error saving wallet:", err)
		}
	}()

	if st := c.w.Status(); !st.Synced || st.Height == 0 {
		fmt.Println("Starting refresh…")
	}
	c.refresh(true)
	if *command != "" {
		c.run(*command)
		return
	}
	c.loop()
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "Error:", err)
	os.Exit(1)
}

// ask — like monero-wallet-cli with no flags: the file name; if it does not exist, it offers to create one.
func (c *cli) ask() {
	name, err := walletcmd.ReadLine("Specify wallet file name (e.g., MyWallet). If the wallet doesn't exist, it will be created.\nWallet file name (or Ctrl-C to quit): ")
	if err != nil || strings.TrimSpace(name) == "" {
		fatal(errors.New("no wallet file name"))
	}
	name = strings.TrimSpace(name)
	if _, err := os.Stat(name); err == nil {
		c.open(name)
		return
	}
	ans, _ := walletcmd.ReadLine(fmt.Sprintf("No wallet found with that name. Confirm creation of new wallet named: %s\n(Y/Yes/N/No): ", name))
	if !yes(ans) {
		os.Exit(0)
	}
	mode, _ := walletcmd.ReadLine("Create a new wallet (1) or restore from 24 words (2)? [1]: ")
	c.create(name, strings.TrimSpace(mode) == "2", 0)
}

func yes(s string) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	return s == "y" || s == "yes"
}

func (c *cli) open(path string) {
	for tries := 0; tries < 3; tries++ {
		password := os.Getenv("POWM_WALLET_PASSWORD")
		if password == "" {
			var err error
			if password, err = walletcmd.ReadPassword("Wallet password: "); err != nil {
				fatal(err)
			}
		}
		fmt.Println("Opening wallet…")
		w, err := wallet.Open(path, password, c.params)
		if err == nil {
			c.w = w
			fmt.Printf("Opened wallet: %s\n", path)
			return
		}
		if !errors.Is(err, wallet.ErrWrongPassword) || os.Getenv("POWM_WALLET_PASSWORD") != "" {
			fatal(err)
		}
		fmt.Println("Wrong password.")
	}
	fatal(wallet.ErrWrongPassword)
}

func (c *cli) create(path string, restore bool, restoreHeight uint64) {
	if _, err := os.Stat(path); err == nil {
		fatal(fmt.Errorf("%s already exists — refusing to overwrite it", path))
	}
	var mnemonic string
	if restore {
		for {
			line, err := walletcmd.ReadLine("Enter your 24 words (the first 4 letters of each are enough):\n")
			if err != nil {
				fatal(err)
			}
			if _, err := bip39.Entropy(line); err != nil {
				fmt.Println(err)
				continue
			}
			mnemonic = line
			break
		}
		if restoreHeight == 0 {
			h, _ := walletcmd.ReadLine("Restore from specific blockchain height (optional, default 0): ")
			if v, err := strconv.ParseUint(strings.TrimSpace(h), 10, 64); err == nil {
				restoreHeight = v
			}
		}
	}
	password, err := walletcmd.NewPassword()
	if err != nil {
		fatal(err)
	}
	fmt.Println("Generating wallet… (key derivation takes a moment)")
	if restore {
		c.w, err = wallet.Restore(path, password, mnemonic, c.params, restoreHeight)
		if err != nil {
			fatal(err)
		}
		fmt.Printf("Restored wallet: %s\n", path)
		return
	}
	birth := uint64(0)
	if h, _, err := c.node.Tip(); err == nil {
		birth = h // a new wallet has nothing to look for in history
	} else {
		fmt.Println("Node is not reachable — the wallet will scan the chain from the beginning:", err)
	}
	c.w, mnemonic, err = wallet.Create(path, password, c.params, birth)
	if err != nil {
		fatal(err)
	}
	addr, err := c.w.NewAddress("Primary account")
	if err != nil {
		fatal(err)
	}
	fmt.Printf("Generated new wallet: %s\nYour first address: %s\n\n", path, addr)
	printSeed(mnemonic)
}

func printSeed(mnemonic string) {
	fmt.Println("NOTE: the following 24 words can be used to recover access to your wallet.")
	fmt.Println("Write them down and store them somewhere safe and secure. Anyone who has them")
	fmt.Println("can spend your coins. Imported private keys are NOT covered by these words.")
	fmt.Println()
	ws := strings.Fields(mnemonic)
	for i, w := range ws {
		fmt.Printf("%2d. %-10s", i+1, w)
		if i%6 == 5 {
			fmt.Println()
		}
	}
	fmt.Println()
}

// ------------------------------------------------------------------ the loop

func (c *cli) loop() {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		fmt.Println("\nSaving wallet…")
		c.w.Close()
		os.Exit(0)
	}()
	go func() {
		for range time.Tick(20 * time.Second) {
			c.refresh(false)
		}
	}()
	fmt.Println(`Type "help" for the list of commands.`)
	for {
		line, err := walletcmd.ReadLine(fmt.Sprintf("[wallet %s]: ", c.shortName()))
		if err != nil {
			return
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		if !c.run(line) {
			return
		}
	}
}

func (c *cli) shortName() string {
	st := c.w.Status()
	as := c.w.Addresses()
	if len(as) > 0 {
		return as[0].Address[:len(c.params.Bech32HRP)+7]
	}
	return fmt.Sprint(st.Height)
}

// refresh brings the wallet up to the node's tip and reports incoming payments.
func (c *cli) refresh(verbose bool) {
	res, err := c.w.Sync(c.node)
	c.out.Lock()
	defer c.out.Unlock()
	if err != nil {
		if verbose {
			fmt.Println("Refresh failed:", err)
		}
		return
	}
	if res.RolledBack > 0 {
		fmt.Printf("\nChain reorganization: rolled back %d block(s)\n", res.RolledBack)
	}
	for _, t := range res.Received {
		fmt.Printf("\nHeight %d, txid %x, received %s POWM\n", t.Height, t.ID, wallet.FormatAmount(t.Net()))
	}
	if verbose {
		fmt.Printf("Refresh done, wallet height %d\n", res.To)
	}
}

// run executes one command. Returns false to exit.
func (c *cli) run(line string) bool {
	f := strings.Fields(line)
	cmd, args := f[0], f[1:]
	c.out.Lock()
	defer c.out.Unlock()
	var err error
	switch cmd {
	case "help":
		c.help()
	case "exit", "quit":
		return false
	case "balance":
		c.balance()
	case "address":
		err = c.address(args)
	case "transfer":
		c.out.Unlock()
		err = c.transfer(args)
		c.out.Lock()
	case "show_transfers":
		c.transfers()
	case "refresh":
		c.out.Unlock()
		c.refresh(true)
		c.out.Lock()
	case "status":
		st := c.w.Status()
		tip, _, terr := c.node.Tip()
		if terr != nil {
			fmt.Printf("Wallet height %d, node not reachable: %v\n", st.Height, terr)
		} else {
			fmt.Printf("Refreshed %d/%d, node %s\n", st.Height, tip, c.params.Name)
		}
	case "seed":
		err = c.withPassword(func() { printSeed(c.w.Mnemonic()) })
	case "export_key":
		if len(args) != 1 {
			err = errors.New("usage: export_key <address>")
			break
		}
		err = c.withPassword(func() {
			k, kerr := c.w.DumpPrivKey(args[0])
			if kerr != nil {
				fmt.Println("Error:", kerr)
				return
			}
			fmt.Println(k)
		})
	case "import_key":
		c.out.Unlock()
		err = c.importKey(args)
		c.out.Lock()
	case "rescan":
		from := uint64(0)
		if len(args) == 1 {
			from, err = strconv.ParseUint(args[0], 10, 64)
			if err != nil {
				break
			}
		}
		if err = c.w.Rescan(from); err == nil {
			c.out.Unlock()
			c.refresh(true)
			c.out.Lock()
		}
	case "set_label":
		if len(args) < 2 {
			err = errors.New("usage: set_label <address> <label>")
			break
		}
		err = c.w.SetLabel(args[0], strings.Join(args[1:], " "))
	case "password":
		err = c.changePassword()
	case "save":
		if err = c.w.Save(); err == nil {
			fmt.Println("Wallet saved.")
		}
	default:
		fmt.Printf("Unknown command %q. Type \"help\".\n", cmd)
	}
	if err != nil {
		fmt.Println("Error:", err)
	}
	return true
}

func (c *cli) help() {
	fmt.Print(`Commands:
  address                       show your addresses
  address new [label]           create a new receiving address
  balance                       show balance
  transfer <address> <amount> [<address> <amount> …]
                                send POWM (amount like 1.5), shows the fee and asks to confirm
  show_transfers                transaction history
  refresh                       fetch new blocks from the node now
  status                        wallet and node height
  seed                          show the 24 recovery words (asks for password)
  export_key <address>          show the private key of an address (asks for password)
  import_key <key> [label]      import a private key (powmsec1…) and rescan
  rescan [height]               rescan the chain from a height (default: wallet birth)
  set_label <address> <label>   name an address
  password                      change the wallet password
  save                          save the wallet now
  exit                          save and quit
`)
}

func (c *cli) balance() {
	b := c.w.Balance(1)
	fmt.Printf("Balance: %s POWM, unlocked balance: %s POWM\n", wallet.FormatAmount(b.Total()), wallet.FormatAmount(b.Available()))
	if b.Immature > 0 {
		fmt.Printf("  mining rewards maturing: %s POWM\n", wallet.FormatAmount(b.Immature))
	}
	if b.Incoming > 0 {
		fmt.Printf("  of which unconfirmed change (spendable now): %s POWM\n", wallet.FormatAmount(b.Incoming))
	}
}

func (c *cli) address(args []string) error {
	if len(args) > 0 && args[0] == "new" {
		a, err := c.w.NewAddress(strings.Join(args[1:], " "))
		if err != nil {
			return err
		}
		fmt.Println(a)
		return nil
	}
	for _, a := range c.w.Addresses() {
		kind := fmt.Sprintf("%4d", a.Index)
		if a.Imported {
			kind = "imp."
		}
		fmt.Printf("%s  %s  %s\n", kind, a.Address, a.Label)
	}
	return nil
}

func (c *cli) transfer(args []string) error {
	if len(args) < 2 || len(args)%2 != 0 {
		return errors.New("usage: transfer <address> <amount> [<address> <amount> …]")
	}
	var pays []wallet.Payment
	var total int64
	for i := 0; i < len(args); i += 2 {
		to, err := c.w.DecodeAddress(args[i])
		if err != nil {
			return err
		}
		v, err := wallet.ParseAmount(args[i+1])
		if err != nil {
			return err
		}
		pays = append(pays, wallet.Payment{To: to, Amount: v})
		total += v
	}
	tx, fee, err := c.w.Build(pays, 0)
	if err != nil {
		return err
	}
	ans, _ := walletcmd.ReadLine(fmt.Sprintf("Sending %s POWM. The transaction fee is %s POWM.\nIs this okay? (Y/Yes/N/No): ",
		wallet.FormatAmount(total), wallet.FormatAmount(fee)))
	if !yes(ans) {
		fmt.Println("Transaction cancelled.")
		return nil
	}
	id := tx.TxID()
	err = c.w.Submit(c.node, tx)
	if errors.Is(err, wallet.ErrBroadcastUnknown) {
		// Not a failure: the wallet holds the transaction and keeps sending it. Saying "error" here would
		// invite the person to send the same payment a second time.
		fmt.Printf("The node did not confirm receiving the transaction (%v).\n"+
			"It is saved in the wallet and will be sent again — do NOT repeat the transfer. Transaction %s\n",
			err, hex.EncodeToString(id[:]))
		return nil
	}
	if err != nil {
		return err
	}
	fmt.Printf("Transaction successfully submitted, transaction %s\n", hex.EncodeToString(id[:]))
	return nil
}

func (c *cli) transfers() {
	txs := c.w.Transactions()
	if len(txs) == 0 {
		fmt.Println("No transfers yet.")
		return
	}
	for i := len(txs) - 1; i >= 0; i-- { // from the oldest, like monero
		t := txs[i]
		dir, amount := "in ", t.Net()
		if t.Spent > 0 {
			dir = "out"
		}
		if t.Coinbase {
			dir = "blk"
		}
		height := "pending"
		if t.Height > 0 {
			height = strconv.FormatUint(t.Height, 10)
		}
		when := time.Unix(t.Time, 0).UTC().Format("2006-01-02 15:04")
		fmt.Printf("%8s %s %s %20s  %x", height, dir, when, wallet.FormatAmount(amount), t.ID[:8])
		if t.Spent > 0 {
			for _, o := range t.Outputs {
				if !o.Mine {
					fmt.Printf("  → %s %s", o.Address, wallet.FormatAmount(o.Value))
				}
			}
			if t.Fee >= 0 {
				fmt.Printf("  fee %s", wallet.FormatAmount(t.Fee))
			}
		}
		fmt.Println()
	}
}

func (c *cli) withPassword(fn func()) error {
	p, err := walletcmd.ReadPassword("Wallet password: ")
	if err != nil {
		return err
	}
	if !c.w.CheckPassword(p) {
		return wallet.ErrWrongPassword
	}
	fn()
	return nil
}

func (c *cli) importKey(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: import_key <powmsec1…> [label]")
	}
	addr, need, err := c.w.ImportPrivKey(args[0], strings.Join(args[1:], " "), 0)
	if err != nil {
		return err
	}
	fmt.Printf("Imported %s\nNOTE: this key is NOT covered by your 24 words — back up the wallet file.\n", addr)
	if need {
		fmt.Println("Rescanning the chain for its coins…")
		if err := c.w.Rescan(0); err != nil {
			return err
		}
		c.refresh(true)
	}
	return nil
}

func (c *cli) changePassword() error {
	old, err := walletcmd.ReadPassword("Current password: ")
	if err != nil {
		return err
	}
	nw, err := walletcmd.NewPassword()
	if err != nil {
		return err
	}
	if err := c.w.ChangePassword(old, nw); err != nil {
		return err
	}
	fmt.Println("Password changed.")
	return nil
}
