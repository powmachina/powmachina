package chain

import (
	"fmt"
	"sort"

	"powmachina/internal/mh"
)

// Fork activation.
//
// A consensus rule that did not exist at the genesis starts at a height, and the height is written
// in the network's parameters (NetParams.Forks). Code that depends on such a rule asks
// `params.Active(fork, height)` and nothing else — no rule is switched by a clock, by a vote or
// by whoever is connected, so two nodes with the same binary always agree on which rules a block
// is checked against.
//
// Every network has its own table. A fork is given a height on regtest first, then on testnet,
// and on mainnet only after it has run there.
//
// Miners additionally say in the header which forks their software is ready for
// (BlockVersion). The node only counts that (ForkStatus) — it is what the height is chosen by,
// it activates nothing. An automatic threshold was left out on purpose: the two lanes have a
// guaranteed share of blocks each, so a threshold would have to hold in each lane separately,
// and a rule that fixes a security hole must not be something a part of the hashrate can hold back.

// Fork names a consensus rule added after the genesis. The number is also the bit the fork is
// signalled with, so it stays below MaxForks and is never reused while the fork is not active
// on every network.
type Fork uint8

const (
	// MaxForks is how many forks can be signalled at once: the upper half of the header version.
	MaxForks = 16
	// VersionBase is the header version itself, the lower half of the field.
	VersionBase uint32 = 1
	// versionSignalShift is where the signal bits start.
	versionSignalShift = 16
	// SignalWindow is how many of the most recent blocks the signalling is counted over
	// (1 000 blocks ≈ 33 h).
	SignalWindow = 1000
)

const (
	// ForkWitnessCommitment: the coinbase commits to the witnesses of the block (witness.go).
	ForkWitnessCommitment Fork = 0
)

// forkNames are the forks this binary knows. A fork that is not here is not signalled and its
// bit in a header counts as unknown.
var forkNames = map[Fork]string{
	ForkWitnessCommitment: "witness-commitment",
}

func (f Fork) String() string {
	if name, ok := forkNames[f]; ok {
		return name
	}
	return fmt.Sprintf("fork-%d", uint8(f))
}

// KnownForks returns the forks this binary knows, in the order of their bits.
func KnownForks() []Fork {
	out := make([]Fork, 0, len(forkNames))
	for f := range forkNames {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// ForkAt is one line of a network's fork table: the rule holds from this height on, the block at
// that height included.
type ForkAt struct {
	Fork   Fork
	Height uint64
}

// ForkHeight returns the height a fork starts at on this network. A fork that is not in the
// table has no height and is never active.
func (p *NetParams) ForkHeight(f Fork) (uint64, bool) {
	for _, at := range p.Forks {
		if at.Fork == f {
			return at.Height, true
		}
	}
	return 0, false
}

// Active says whether a fork's rule holds for the block at the given height.
func (p *NetParams) Active(f Fork, height uint64) bool {
	at, ok := p.ForkHeight(f)
	return ok && height >= at
}

// BlockVersion is the version a block mined at the given height carries: the base and one bit
// for every fork this binary knows and that is not active at that height yet. Once a fork is
// active its bit says nothing and is left out.
func (p *NetParams) BlockVersion(height uint64) uint32 {
	v := VersionBase
	for f := range forkNames {
		if f < MaxForks && !p.Active(f, height) {
			v |= 1 << (versionSignalShift + uint32(f))
		}
	}
	return v
}

// SignalsFork says whether a header version signals readiness for a fork.
func SignalsFork(version uint32, f Fork) bool {
	return f < MaxForks && version&(1<<(versionSignalShift+uint32(f))) != 0
}

// checkForks verifies a fork table: every fork fits a signal bit and has one height.
func (p *NetParams) checkForks() error {
	seen := map[Fork]bool{}
	for _, at := range p.Forks {
		if at.Fork >= MaxForks {
			return fmt.Errorf("chain: fork %d on %s does not fit a signal bit", at.Fork, p.Name)
		}
		if seen[at.Fork] {
			return fmt.Errorf("chain: fork %s is in the %s table twice", at.Fork, p.Name)
		}
		seen[at.Fork] = true
	}
	return nil
}

// ForkState is one signal bit as the chain sees it.
type ForkState struct {
	Fork Fork
	// Known: this binary has the rule. A bit that is not known and is being signalled means the
	// network is getting ready for something this node will not follow.
	Known bool
	// Scheduled: the fork has a height on this network.
	Scheduled bool
	Height    uint64
	// Active: the rule holds for the next block.
	Active bool
	// Signalling is the number of blocks in the window that carry the bit, per lane.
	Signalling [len(mh.Lanes)]int
}

// ForkStatus is the fork table of the network together with what the miners signal.
type ForkStatus struct {
	TipHeight uint64
	// Window is how many blocks were counted; a young chain has fewer than SignalWindow.
	Window int
	// Blocks is the number of blocks in the window per lane — the denominator for Signalling.
	Blocks [len(mh.Lanes)]int
	// Forks holds every fork that is known, scheduled or signalled by at least one block.
	Forks []ForkState
}

// ForkStatus counts the signal bits over the most recent blocks of the active branch.
func (hc *HeaderChain) ForkStatus() ForkStatus {
	hc.mu.RLock()
	defer hc.mu.RUnlock()

	out := ForkStatus{}
	if hc.tip == nil {
		return out
	}
	out.TipHeight = hc.tip.height

	var signalling [MaxForks][len(mh.Lanes)]int
	for n := hc.tip; n != nil && out.Window < SignalWindow; n = hc.byID[n.header.PrevBlock] {
		if n.id == hc.genesis {
			break
		}
		lane := int(n.header.Lane)
		if lane >= len(mh.Lanes) {
			continue
		}
		out.Window++
		out.Blocks[lane]++
		for f := Fork(0); f < MaxForks; f++ {
			if SignalsFork(n.header.Version, f) {
				signalling[f][lane]++
			}
		}
	}

	for f := Fork(0); f < MaxForks; f++ {
		st := ForkState{Fork: f, Signalling: signalling[f]}
		_, st.Known = forkNames[f]
		st.Height, st.Scheduled = hc.params.ForkHeight(f)
		st.Active = hc.params.Active(f, out.TipHeight+1)
		var signalled bool
		for _, n := range st.Signalling {
			signalled = signalled || n > 0
		}
		if st.Known || st.Scheduled || signalled {
			out.Forks = append(out.Forks, st)
		}
	}
	return out
}
