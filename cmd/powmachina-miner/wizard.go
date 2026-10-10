package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"golang.org/x/term"

	"powmachina/internal/address"
	"powmachina/internal/chain"
	"powmachina/internal/gpu"
	"powmachina/internal/miner"
	"powmachina/internal/walletcmd"
)

// The setup wizard: what a person gets when they start the miner with no arguments — on Windows that
// is a double click. It asks the few things the miner cannot guess and turns the answers into the same
// flags the command line takes, so there is one code path behind both and nothing the wizard can do
// that a flag cannot.

// confName is the settings file next to the binary: one flag per line, `#` starts a comment.
const confName = "powmachina-miner.conf"

// wizardDefaultNetwork is the answer offered for the network question. Mainnet has no genesis yet, so
// offering it by default would send every first-time user into a network that does not exist. Flip it
// when mainnet launches.
const wizardDefaultNetwork = "testnet"

// wizardEnv is what the wizard knows about the machine before it asks anything.
type wizardEnv struct {
	Threads  int      // the recommended number of lane C threads
	MaxCPU   int      // logical CPUs
	GPUs     []string // the cards the plugin sees
	GPUNote  string   // why there are none (shown instead of the GPU choice)
	Hostname string   // the default worker name
	HasToken bool     // POWM_RPC_TOKEN is already in the environment
}

// wizardResult is the outcome: the flags, the node token for solo mining (never written to disk), and
// whether to save the flags.
type wizardResult struct {
	Args  []string
	Token string
	Save  bool
}

type asker struct {
	in  *bufio.Reader
	out io.Writer
}

// line asks a question and returns the trimmed answer, or def when the answer is empty.
func (a *asker) line(prompt, def string) (string, error) {
	if def != "" {
		fmt.Fprintf(a.out, "%s [%s]: ", prompt, def)
	} else {
		fmt.Fprintf(a.out, "%s: ", prompt)
	}
	s, err := a.in.ReadString('\n')
	if err != nil && s == "" {
		return "", errors.New("the input ended before the setup was finished")
	}
	if s = strings.TrimSpace(s); s == "" {
		s = def
	}
	return s, nil
}

// choice asks until it gets a number from 1 to n.
func (a *asker) choice(prompt string, n, def int) (int, error) {
	for {
		s, err := a.line(prompt, strconv.Itoa(def))
		if err != nil {
			return 0, err
		}
		if v, err := strconv.Atoi(s); err == nil && v >= 1 && v <= n {
			return v, nil
		}
		fmt.Fprintf(a.out, "  Please type a number from 1 to %d.\n", n)
	}
}

func runWizard(in io.Reader, out io.Writer, env wizardEnv) (wizardResult, error) {
	a := &asker{in: bufio.NewReader(in), out: out}
	var res wizardResult
	p := func(format string, args ...any) { fmt.Fprintf(out, format+"\n", args...) }

	p("")
	p("PowMachina miner — setup")
	p("")
	p("A few questions, then the miner starts. Press Enter to take the answer in [brackets].")

	// The network.
	p("")
	p("1) Which network?")
	p("     1  mainnet")
	p("     2  testnet — coins without value, for trying things out")
	def := 1
	if wizardDefaultNetwork == "testnet" {
		def = 2
	}
	n, err := a.choice("   Network", 2, def)
	if err != nil {
		return res, err
	}
	params := chain.MainNet
	if n == 2 {
		params = chain.TestNet
		res.Args = append(res.Args, "--testnet")
	}

	// Pool or solo.
	p("")
	p("2) How do you want to mine?")
	p("     1  pool — you mine together with others and are paid for your share of the work.")
	p("               You need the pool's address.")
	p("     2  solo — against your own node (powmachinad). A block you find is all yours,")
	p("               but on your own they come rarely.")
	mode, err := a.choice("   Mode", 2, 1)
	if err != nil {
		return res, err
	}
	if mode == 1 {
		p("")
		p("   The pool's address is on the pool's website. It looks like")
		p("     stratum+tcp://pool.example.com:%d", params.StratumPort)
		p("   Typing just pool.example.com is enough — the rest is filled in.")
		for {
			s, err := a.line("   Pool address", "")
			if err != nil {
				return res, err
			}
			url, err := poolURL(s, params.StratumPort)
			if err != nil {
				p("  %v", err)
				continue
			}
			res.Args = append(res.Args, "--pool="+url)
			break
		}
	} else {
		p("")
		p("   The node has to be running with its RPC on (powmachinad -rpc …). If it runs on this")
		p("   machine, just press Enter.")
		defNode := fmt.Sprintf("127.0.0.1:%d", params.RPCPort)
		for {
			s, err := a.line("   Node address", defNode)
			if err != nil {
				return res, err
			}
			hp, err := hostPort(s, params.RPCPort)
			if err != nil {
				p("  %v", err)
				continue
			}
			if hp != defNode {
				res.Args = append(res.Args, "--node="+hp)
			}
			break
		}
		if !env.HasToken {
			p("")
			p("   The node's RPC token — the value of POWM_RPC_TOKEN the node was started with.")
			p("   It is asked for at every start and is not saved. Press Enter if the node has none.")
			if res.Token, err = a.line("   RPC token", ""); err != nil {
				return res, err
			}
		}
	}

	// The address.
	p("")
	p("3) Where should the rewards go?")
	p("   Your PowMachina address, starting with %s1… — the desktop wallet shows it under", params.Bech32HRP)
	p("   Receive, and powmachina-wallet-cli prints it with the `address` command.")
	for {
		s, err := a.line("   Address", "")
		if err != nil {
			return res, err
		}
		if err := checkAddress(params, s); err != nil {
			p("  %v", err)
			continue
		}
		res.Args = append(res.Args, "--address="+s)
		break
	}

	// The worker name only means something to a pool.
	if mode == 1 {
		p("")
		p("4) A name for this machine, so you can tell your rigs apart on the pool.")
		for {
			s, err := a.line("   Worker name", workerName(env.Hostname))
			if err != nil {
				return res, err
			}
			if s != workerName(s) {
				p("  Letters, digits, - and _ only, please.")
				continue
			}
			res.Args = append(res.Args, "--worker="+s)
			break
		}
	}

	// What mines.
	p("")
	p("5) What should mine?")
	p("   The CPU mines lane C, a graphics card mines lane G; both pay.")
	p("   CPU: %d threads available, %d recommended", env.MaxCPU, env.Threads)
	useCPU := true
	if len(env.GPUs) == 0 {
		p("   GPU: none — %s", env.GPUNote)
		p("   The CPU will mine.")
		res.Args = append(res.Args, "--gpu=off")
	} else {
		for i, g := range env.GPUs {
			p("   GPU %d: %s", i, g)
		}
		p("     1  CPU and GPU")
		p("     2  GPU only")
		p("     3  CPU only")
		c, err := a.choice("   Mine with", 3, 1)
		if err != nil {
			return res, err
		}
		switch c {
		case 1:
			res.Args = append(res.Args, "--gpu=on")
		case 2:
			res.Args = append(res.Args, "--gpu=on", "--cpu=false")
			useCPU = false
		case 3:
			res.Args = append(res.Args, "--gpu=off")
		}
	}
	if useCPU {
		p("")
		p("   How many CPU threads? Fewer leaves the machine usable for other work; more than the")
		p("   recommended number does not help (each thread needs about 2 MiB of the CPU's cache).")
		for {
			s, err := a.line("   CPU threads", strconv.Itoa(env.Threads))
			if err != nil {
				return res, err
			}
			v, err := strconv.Atoi(s)
			if err != nil || v < 1 || v > env.MaxCPU {
				p("  Please type a number from 1 to %d.", env.MaxCPU)
				continue
			}
			if v != env.Threads {
				res.Args = append(res.Args, "--threads="+strconv.Itoa(v))
			}
			break
		}
	}

	p("")
	p("That is all. The same thing as a command line:")
	p("   powmachina-miner %s", strings.Join(res.Args, " "))
	p("")
	for {
		s, err := a.line("Save these settings, so the miner starts straight away next time? (y/n)", "y")
		if err != nil {
			return res, err
		}
		switch strings.ToLower(s) {
		case "y", "yes":
			res.Save = true
		case "n", "no":
		default:
			continue
		}
		break
	}
	p("")
	return res, nil
}

// poolURL completes what a person typed into a stratum URL: the scheme and the port are optional.
func poolURL(s string, defPort uint16) (string, error) {
	scheme := "stratum+tcp://"
	for _, sc := range []string{"stratum+tcp://", "stratum+ssl://"} {
		if strings.HasPrefix(strings.ToLower(s), sc) {
			scheme, s = sc, s[len(sc):]
		}
	}
	if s == "" {
		return "", errors.New("The pool address is needed to mine on a pool.")
	}
	if strings.Contains(s, "://") {
		return "", errors.New("Only stratum+tcp:// and stratum+ssl:// are understood.")
	}
	hp, err := hostPort(strings.TrimRight(s, "/"), defPort)
	if err != nil {
		return "", err
	}
	return scheme + hp, nil
}

// hostPort accepts host or host:port and returns host:port.
func hostPort(s string, defPort uint16) (string, error) {
	if strings.ContainsAny(s, " /") || s == "" {
		return "", fmt.Errorf("%q does not look like host:port.", s)
	}
	host, port, err := net.SplitHostPort(s)
	if err != nil {
		// No port: a bare name, an IPv4 address or a bracketed IPv6 one.
		host, port = strings.Trim(s, "[]"), strconv.Itoa(int(defPort))
		if strings.Contains(s, ":") && !strings.HasPrefix(s, "[") {
			return "", fmt.Errorf("%q does not look like host:port.", s)
		}
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 || host == "" {
		return "", fmt.Errorf("%q does not look like host:port.", s)
	}
	return net.JoinHostPort(host, port), nil
}

// checkAddress says what is wrong with an address in words a person can act on.
func checkAddress(params chain.NetParams, s string) error {
	if s == "" {
		return errors.New("An address is needed — without one the rewards have nowhere to go.")
	}
	v, prog, err := address.Decode(params.Bech32HRP, s)
	if err != nil {
		for _, other := range []chain.NetParams{chain.MainNet, chain.TestNet, chain.RegTest} {
			if other.Name != params.Name && strings.HasPrefix(strings.ToLower(s), other.Bech32HRP+"1") {
				return fmt.Errorf("That is a %s address; on %s an address starts with %s1…", other.Name, params.Name, params.Bech32HRP)
			}
		}
		return fmt.Errorf("That is not a valid %s address (%v). Check it for a typo — copy and paste is safest.", params.Name, err)
	}
	if v != chain.ScriptVersionBasic || len(prog) != chain.PubkeyBytes {
		return fmt.Errorf("That address type cannot receive mining rewards; use an ordinary %s1… address.", params.Bech32HRP)
	}
	return nil
}

// workerName reduces a string to what a stratum login can carry after the dot.
func workerName(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "rig1"
	}
	return b.String()
}

// holdWindow is set when the miner was started with no arguments on Windows — most likely by a double
// click, where the console closes the moment the program exits and takes the error message with it.
var holdWindow bool

// tokenAsked says the setup questions already asked for the node's RPC token (the answer may be empty).
var tokenAsked bool

// hold waits for Enter before the window disappears.
func hold() {
	if holdWindow {
		fmt.Print("\nPress Enter to close this window.")
		walletcmd.Stdin.ReadString('\n')
	}
}

// die is logger.Fatalf that leaves the message readable in a double-clicked window.
func die(logger *log.Logger, format string, args ...any) {
	logger.Printf(format, args...)
	hold()
	os.Exit(1)
}

// startupArgs decides where the flags come from. With arguments it is the command line, untouched.
// Without any it is the saved settings, and when there are none — or `--setup` asks for it — the
// setup questions.
func startupArgs(argv []string, logger *log.Logger) []string {
	setup := len(argv) == 1 && (argv[0] == "--setup" || argv[0] == "-setup")
	if len(argv) > 0 && !setup {
		return argv
	}
	tty := term.IsTerminal(int(os.Stdin.Fd()))
	holdWindow = tty && runtime.GOOS == "windows"
	path := confPath()
	if !setup {
		if b, err := os.ReadFile(path); err == nil {
			logger.Printf("settings from %s (run `powmachina-miner --setup` to change them)", path)
			return parseConf(string(b))
		}
	}
	if !tty {
		if setup {
			die(logger, "--setup asks questions and needs a terminal")
		}
		return nil // a script with no arguments gets the usual "missing --address"
	}
	res, err := runWizard(walletcmd.Stdin, os.Stdout, probe())
	if err != nil {
		die(logger, "%v", err)
	}
	tokenAsked = true
	if res.Token != "" {
		os.Setenv("POWM_RPC_TOKEN", res.Token)
	}
	if res.Save {
		if err := os.WriteFile(path, []byte(formatConf(res.Args)), 0o600); err != nil {
			logger.Printf("the settings could not be saved (%v) — the miner starts anyway", err)
		} else {
			logger.Printf("settings saved to %s", path)
		}
	}
	return res.Args
}

// rpcToken is the node's token for solo mining: from the environment, and when the miner was started
// by a person from saved settings — where the token is deliberately not kept — by asking.
func rpcToken() string {
	t := os.Getenv("POWM_RPC_TOKEN")
	if t == "" && !tokenAsked && len(os.Args) == 1 && term.IsTerminal(int(os.Stdin.Fd())) {
		t, _ = walletcmd.ReadLine("The node's RPC token (Enter if it has none): ")
	}
	return t
}

// probe looks at the machine so that the questions can offer real choices.
func probe() wizardEnv {
	env := wizardEnv{Threads: miner.DefaultThreads(), MaxCPU: runtime.NumCPU(), HasToken: os.Getenv("POWM_RPC_TOKEN") != ""}
	env.Hostname, _ = os.Hostname()
	pl, err := gpu.Load("")
	if err != nil {
		env.GPUNote = fmt.Sprintf("the GPU plugin did not load (%v)", err)
		return env
	}
	for _, d := range pl.Devices() {
		env.GPUs = append(env.GPUs, fmt.Sprintf("%s, %.0f GB", d.Name, float64(d.Memory)/1e9))
	}
	if len(env.GPUs) == 0 {
		env.GPUNote = "no NVIDIA card was found"
	}
	return env
}

// confPath is where the settings live: next to the binary, so an unpacked folder stays self-contained.
func confPath() string {
	exe, err := os.Executable()
	if err != nil {
		return confName
	}
	return filepath.Join(filepath.Dir(exe), confName)
}

// parseConf turns the settings file into flags.
func parseConf(text string) []string {
	var args []string
	for _, l := range strings.Split(text, "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
			args = append(args, l)
		}
	}
	return args
}

// formatConf is the settings file for a set of flags.
func formatConf(args []string) string {
	return "# PowMachina miner settings — one flag per line, the same flags as on the command line\n" +
		"# (powmachina-miner --help). Delete this file or run `powmachina-miner --setup` to be asked again.\n" +
		strings.Join(args, "\n") + "\n"
}
