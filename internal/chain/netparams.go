package chain

import (
	"encoding/hex"
	"math/big"

	"powmachina/internal/mh"
)

// NetParams are the parameters of a particular network. A node has to know which network it runs
// on — otherwise regtest and mainnet would share the difficulty floor and the magic bytes, which
// is a source of silent bugs.
//
// Consensus values that are the same for every network (emission, block time, the lane rules)
// stay in params.go as constants.
type NetParams struct {
	Name string
	// Magic distinguishes the networks on the wire.
	Magic [4]byte
	// PowLimitBits is the lowest allowed difficulty of this network (the floor).
	PowLimitBits uint32
	// The ports follow one scheme: the base is **7696 = "POWM" on a phone
	// keypad**, testnet has a 1 in front of it, regtest a 2, and the last digit picks the service —
	// 6 P2P, 7 RPC, 8 the HTTP block mirror, 9 stratum. Monero uses the same prefix scheme.
	//
	// All of them are **below 32768**. The previous ports (41337, 39777, 33771) were in the range
	// Linux assigns outgoing connections from (32768–60999) — after a restart a random connection
	// could take them and the node would not start. The TestPortScheme test guards this.
	P2PPort     uint16
	RPCPort     uint16
	BlocksPort  uint16 // the HTTP mirror of block bodies (-serve-blocks)
	StratumPort uint16 // mining
	// DagProfile are the sizes of lane G's DAG. Zero = the mainnet profile (1 GiB and growing);
	// regtest has the test one (8 MiB) so it can be mined and verified on anything. Both the node
	// and the miner take it from here.
	DagProfile mh.DagProfile
	// WalletRPCPort is powmachina-wallet-rpc — one below the base (7695), so the P2P…stratum
	// scheme stays untouched.
	WalletRPCPort uint16
	// DNSSeeds are the names that return node addresses. They have to be **on different domains
	// and with different people** (SECURITY.md §5) — one domain for every seed fails all at once.
	DNSSeeds []string
	// FixedSeeds are addresses baked into the binary in case DNS fails or is censored.
	FixedSeeds []string

	// Bech32HRP is the address prefix. It is visible on every address, so it carries the network
	// name: `powm1…` on mainnet, `tpowm1…` on testnet, `rpowm1…` on regtest.
	// The prefix is also a protection — an address from another network is refused on decoding.
	Bech32HRP string
	// FinalityWindow is how many blocks below a height make up the quorum that finalizes it
	// (the finality spec). 0 = finality is off and the chain runs on plain PoW.
	FinalityWindow uint64
	// FinalityActive is how recently a key has to have mined to keep its seat (QuorumFor); 0 keeps
	// every key of the window.
	FinalityActive uint64

	// MinChainWork is the smallest cumulative chainwork a chain has to have for the node to treat
	// it as trustworthy (hex, empty = not checked).
	//
	// What it is for: the first blocks of every network are at the difficulty floor, so they can be
	// produced cheaply. A node connecting for the first time has no way to know whether it is
	// getting the real history or a branch somebody ground out on one card in an hour. A checkpoint
	// **refuses** that; MinChainWork is the second safeguard: the node does not report a chain below
	// it as synced, so neither an exchange nor a wallet starts working on it.
	MinChainWork string
	// Checkpoints are blocks baked into the binary at release time: the header at a given height
	// has to have exactly this ID. They protect a new node during its first sync — an attacker
	// cannot slip it a cheap fake chain, because it hits the first checkpoint. Empty before the
	// genesis; every release adds a checkpoint deep below the tip (a fixed one, not a rolling one).
	Checkpoints []Checkpoint
	// Forks are the consensus rules added after the genesis and the height each starts at on this
	// network (forks.go). A rule that is not listed is not active here.
	Forks []ForkAt

	// DevFeeKey is the 32-byte key the dev fee is paid to (SPEC §2.1): the first output of every
	// coinbase while the fee lasts. On mainnet it is one key, generated offline and never on a
	// server; it is filled in before the genesis.
	DevFeeKey []byte
	// DevFeeScript is what the dev fee was paid to before ForkStrictOutputs, when an output did
	// not have to be a key: a placeholder nobody could spend. A network that starts with the fork
	// on never uses it (DevFeeScriptAt).
	DevFeeScript []byte
}

var (
	MainNet = NetParams{
		Name:          "mainnet",
		Magic:         [4]byte{'P', 'W', 'M', '1'},
		PowLimitBits:  MaxTargetBits,
		P2PPort:       7696,
		RPCPort:       7697,
		BlocksPort:    7698,
		StratumPort:   7699,
		WalletRPCPort: 7695,
		// TODO(before genesis): seeds from different operators, on different domains and with
		// different registrars.
		DNSSeeds:   nil,
		FixedSeeds: nil,
		Bech32HRP:  "powm",
		// TODO(before genesis): the dev fee key.
		DevFeeKey: nil,
		// TODO(before genesis): finality quorum keys from independent operators.
		FinalityWindow: FinalityWindow,
		FinalityActive: FinalityActive,
		// A new network starts with every rule on.
		Forks: []ForkAt{
			{Fork: ForkWitnessCommitment, Height: 0},
			{Fork: ForkSignedNetwork, Height: 0},
			{Fork: ForkLockTime, Height: 0},
			{Fork: ForkStrictOutputs, Height: 0},
			{Fork: ForkLaneDifficulty, Height: 0},
		},
	}

	TestNet = NetParams{
		Name:          "testnet",
		Magic:         [4]byte{'P', 'W', 'M', 'T'},
		PowLimitBits:  MaxTargetBits,
		P2PPort:       17696,
		RPCPort:       17697,
		BlocksPort:    17698,
		StratumPort:   17699,
		WalletRPCPort: 17695,
		Bech32HRP:     "tpowm",
		// The testnet seeds are on one domain and under one administrator — on mainnet it must not
		// be like that (SECURITY.md §5), here it is deliberately simple. The records have to be
		// **DNS only**, never proxied: behind a proxying CDN the peers would get the CDN's addresses
		// and try to open a P2P connection to them.
		DNSSeeds: []string{"seed-t1.powmachina.com", "seed-t2.powmachina.com"},
		// Baked-in addresses are the fallback when DNS fails. They are public nodes that rotate, so
		// they can become invalid — which is why they are only used when DNS gives nothing.
		FixedSeeds:   []string{"2.28.131.131:17696", "37.27.149.235:17696"},
		DevFeeScript: []byte("testnet-dev-fee-placeholder"),
		DevFeeKey:    placeholderKey("testnet"),
		// The same window as mainnet, so testnet exercises exactly those paths (the finality spec).
		FinalityWindow: FinalityWindow,
		FinalityActive: FinalityActive,
		// Both values are produced by `scripts/bake-checkpoint.sh` and are re-baked before every
		// release: the deeper the checkpoint, the less history can be slipped to a node syncing
		// for the first time. The checkpoint sits on a block the finality quorum confirmed and
		// at least ReorgLimit below the tip at baking time — any shallower and a reorg that
		// nodes may still legitimately make would split old binaries from new ones.
		// MinChainWork is taken from the tip, not from the checkpoint: the question it answers
		// is "does this chain have at least as much work as the network had when this binary
		// was made". Until a chain has both, the node does not defend it (anchoredLocked).
		//
		// Empty until the chain is old enough to bake them.
		MinChainWork: "",
		Checkpoints:  nil,
		// A new network starts with every rule on.
		Forks: []ForkAt{
			{Fork: ForkWitnessCommitment, Height: 0},
			{Fork: ForkSignedNetwork, Height: 0},
			{Fork: ForkLockTime, Height: 0},
			{Fork: ForkStrictOutputs, Height: 0},
			{Fork: ForkLaneDifficulty, Height: 0},
		},
	}

	// RegTest has a trivial difficulty so it can be tested without waiting for blocks.
	RegTest = NetParams{
		Name:           "regtest",
		Magic:          [4]byte{'P', 'W', 'M', 'R'},
		PowLimitBits:   RegtestTargetBits,
		P2PPort:        27696,
		RPCPort:        27697,
		BlocksPort:     27698,
		StratumPort:    27699,
		WalletRPCPort:  27695,
		Bech32HRP:      "rpowm",
		DevFeeScript:   []byte("regtest-dev-fee"),
		DevFeeKey:      placeholderKey("regtest"),
		FinalityWindow: RegtestFinalityWindow,
		DagProfile:     mh.TestProfile,
		Forks: []ForkAt{
			{Fork: ForkWitnessCommitment, Height: 0},
			{Fork: ForkSignedNetwork, Height: 0},
			{Fork: ForkLockTime, Height: 0},
			{Fork: ForkStrictOutputs, Height: 0},
			{Fork: ForkLaneDifficulty, Height: 0},
		},
	}
)

// DevFeeScriptAt returns the script the dev fee output has to carry in a block at this height.
func (p *NetParams) DevFeeScriptAt(height uint64) []byte {
	if p.Active(ForkStrictOutputs, height) {
		return p.DevFeeKey
	}
	return p.DevFeeScript
}

// placeholderKey is a dev fee "key" for a network whose coins are worth nothing: 32 bytes that
// are a hash and not a key anybody generated, so whatever is paid there stays there.
func placeholderKey(network string) []byte {
	h := mh.TaggedHash("PowMachina/dev-fee-placeholder/v0", []byte(network))
	return h[:]
}

// MinWork returns MinChainWork as a number. Empty or corrupted = no check.
func (p NetParams) MinWork() *big.Int {
	if p.MinChainWork == "" {
		return nil
	}
	w, ok := new(big.Int).SetString(p.MinChainWork, 16)
	if !ok {
		return nil
	}
	return w
}

// Checkpoint is a baked-in block: a height and its ID.
type Checkpoint struct {
	Height uint64
	ID     [32]byte
}

// mustID converts the hex form of a block ID into bytes — for baking checkpoints into the source.
//
// It panics on bad input on purpose: the value is a literal in the source, so a typo should stop
// startup rather than show up a month later as "the block does not match the checkpoint". The byte
// order is the one RPC and `Header.ID()` print — nothing is reversed anywhere.
func mustID(s string) [32]byte {
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 32 {
		panic("netparams: bad checkpoint ID " + s)
	}
	var id [32]byte
	copy(id[:], b)
	return id
}

func (p NetParams) checkpointAt(height uint64) ([32]byte, bool) {
	for _, cp := range p.Checkpoints {
		if cp.Height == height {
			return cp.ID, true
		}
	}
	return [32]byte{}, false
}
