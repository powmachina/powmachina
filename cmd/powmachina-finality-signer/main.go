// powmachina-finality-signer — a signer of the finality quorum.
//
// A quorum key **does not belong on a machine running a node or a pool** (the finality spec §5),
// so signing is a process of its own: it asks the node about the chain over RPC, signs the block
// at depth FinalityDepth and sends the attestation back (`submitattestation`). The node verifies
// it by the signature against the network's quorum — not by trusting whoever called RPC.
//
//	powmachina-finality-signer -genkey > signer3.key          # makes a key, prints the pubkey
//	powmachina-finality-signer -testnet -index 3 -key signer3.key -node 10.0.0.1:17697
//
// # What this program must never do
//
// Sign two different blocks at the same height. It is the one thing a quorum must not do, and
// there is a proof for it (`Equivocation`) that turns finality off for that height across the
// whole network. That is why the signer has a **state file**: the last signed height and its
// block are written to disk (atomically, `fsync`) **before** the attestation is sent. After a
// crash the worst case is therefore a missing attestation — a second one never comes into
// existence.
package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"powmachina/internal/chain"
	"powmachina/internal/nodeclient"
	"powmachina/internal/p2p"
	"powmachina/internal/walletcmd"
)

func main() {
	genkey := flag.Bool("genkey", false, "make a new key: the seed on stdout, the public key on stderr")
	keyPath := flag.String("key", "", "key file (hex seed, 64 characters, permissions 0600)")
	index := flag.Int("index", -1, "index in the network's quorum (0..8)")
	nodeAddr := flag.String("node", "", "node RPC (host:port); defaults to localhost for the network")
	statePath := flag.String("state", "", "state file against double signing (defaults to <key>.state)")
	every := flag.Duration("interval", 30*time.Second, "how often to look at the chain")
	testnet := flag.Bool("testnet", false, "the testnet network")
	regtest := flag.Bool("regtest", false, "the regtest network")
	flag.Parse()

	if *genkey {
		if err := generateKey(); err != nil {
			log.Fatal(err)
		}
		return
	}

	params, err := walletcmd.Network(*testnet, *regtest)
	if err != nil {
		log.Fatal(err)
	}
	if params.SignerSet() == nil {
		log.Fatalf("network %s has no finality quorum — there is nothing to sign", params.Name)
	}
	if *index < 0 || *index >= len(params.FinalitySigners) {
		log.Fatalf("-index has to be 0..%d (the quorum of network %s)", len(params.FinalitySigners)-1, params.Name)
	}
	if *keyPath == "" {
		log.Fatal("-key is required (a file with a hex seed)")
	}

	priv, err := loadKey(*keyPath)
	if err != nil {
		log.Fatal(err)
	}
	if err := checkKeyMatchesIndex(params, *index, priv); err != nil {
		log.Fatal(err)
	}

	st := *statePath
	if st == "" {
		st = *keyPath + ".state"
	}
	last, err := loadState(st)
	if err != nil {
		log.Fatal(err)
	}

	client := nodeclient.New(walletcmd.DaemonAddress(params, *nodeAddr), os.Getenv("POWM_RPC_TOKEN"))
	s := &signer{params: params, index: uint8(*index), priv: priv, client: client,
		statePath: st, last: last}

	log.Printf("signer %d on network %s, node %s, depth %d blocks, interval %s",
		*index, params.Name, walletcmd.DaemonAddress(params, *nodeAddr), chain.FinalityDepth, *every)
	if last.Height > 0 {
		log.Printf("last signed: height %d (%x)", last.Height, last.BlockID[:6])
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	t := time.NewTicker(*every)
	defer t.Stop()
	s.once(ctx) // do not wait for the first tick
	for {
		select {
		case <-ctx.Done():
			log.Println("shutting down")
			return
		case <-t.C:
			s.once(ctx)
		}
	}
}

// nodeRPC is what the signer needs from the node. An interface, so the protection against
// double signing can be exercised without running a whole node.
type nodeRPC interface {
	Tip() (uint64, [32]byte, error)
	BlockHashAt(ctx context.Context, height uint64) ([32]byte, error)
	Finality(ctx context.Context) (uint64, bool, error)
	SubmitAttestation(ctx context.Context, attHex string) (string, error)
}

type signer struct {
	params    chain.NetParams
	index     uint8
	priv      ed25519.PrivateKey
	client    nodeRPC
	statePath string
	last      state
}

// state is what protects against double signing.
type state struct {
	Height  uint64 `json:"height"`
	BlockID [32]byte
}

type stateFile struct {
	Height  uint64 `json:"height"`
	BlockID string `json:"block_id"`
}

// once is one round: find the tip, find the block at the finality depth, sign it and send it.
func (s *signer) once(ctx context.Context) {
	height, _, err := s.client.Tip()
	if err != nil {
		log.Printf("the node did not answer: %v", err)
		return
	}
	if height < chain.FinalityDepth {
		return // the chain is not long enough yet
	}
	target := height - chain.FinalityDepth
	if target <= s.last.Height {
		s.resend(ctx)
		return // we have already signed this height
	}
	id, err := s.client.BlockHashAt(ctx, target)
	if err != nil {
		log.Printf("height %d cannot be read: %v", target, err)
		return
	}

	// The state is written BEFORE sending. After a crash an attestation is missing rather than a second one existing.
	next := state{Height: target, BlockID: id}
	if err := saveState(s.statePath, next); err != nil {
		log.Printf("the state cannot be saved, not signing: %v", err)
		return
	}
	s.last = next

	att := chain.Sign(s.priv, s.index, target, id)
	raw, err := p2p.EncodeAttestation(att)
	if err != nil {
		log.Printf("the attestation cannot be encoded: %v", err)
		return
	}
	res, err := s.client.SubmitAttestation(ctx, hex.EncodeToString(raw))
	if err != nil {
		log.Printf("the node did not accept the attestation: %v", err)
		return
	}
	log.Printf("signed height %d (%x)%s", target, id[:6], res)
}

// checkKeyMatchesIndex verifies that the key belongs to the index in the network's quorum.
//
// Without this check the signer would send attestations the quorum drops — and that would only
// show up as finality never advancing, which is exactly the kind of fault one hunts for hours.
func checkKeyMatchesIndex(params chain.NetParams, index int, priv ed25519.PrivateKey) error {
	if index < 0 || index >= len(params.FinalitySigners) {
		return fmt.Errorf("index %d is outside the quorum of network %s (0..%d)",
			index, params.Name, len(params.FinalitySigners)-1)
	}
	want := params.FinalitySigners[index]
	got := priv.Public().(ed25519.PublicKey)
	if !got.Equal(want) {
		return fmt.Errorf("the key does not belong to index %d: the quorum has %x… there, the key is %x…",
			index, want[:8], got[:8])
	}
	return nil
}

// resend sends the last signed attestation again if the network's finality has not reached its
// height yet.
//
// Why this is needed: finality state lives only in the node's memory. When the whole network
// restarts (common while a new binary is being rolled out), the attestations are lost — and
// the signer would never send them again on its own, because it already has that height signed.
// Finality would then stand still until the chain grew by another FinalityDepth blocks.
//
// Repeating is safe: it is the **same** attestation (Ed25519 is deterministic), so not a second
// signature over a different header. To the node it is a duplicate, which it does not relay.
func (s *signer) resend(ctx context.Context) {
	if s.last.Height == 0 {
		return
	}
	have, ok, err := s.client.Finality(ctx)
	if err != nil || (ok && have >= s.last.Height) {
		return
	}
	att := chain.Sign(s.priv, s.index, s.last.Height, s.last.BlockID)
	raw, err := p2p.EncodeAttestation(att)
	if err != nil {
		return
	}
	if _, err := s.client.SubmitAttestation(ctx, hex.EncodeToString(raw)); err != nil {
		log.Printf("resending height %d failed: %v", s.last.Height, err)
		return
	}
	log.Printf("height %d sent again (the network's finality has not reached it yet)", s.last.Height)
}

func generateKey() error {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	seed := priv.Seed()
	fmt.Println(hex.EncodeToString(seed))
	fmt.Fprintf(os.Stderr, "public key (for chain.NetParams.FinalitySigners): %s\n",
		hex.EncodeToString(pub))
	return nil
}

func loadKey(path string) (ed25519.PrivateKey, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s has permissions %o — a quorum key has to be 0600", path, info.Mode().Perm())
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	seed, err := hex.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil {
		return nil, fmt.Errorf("%s is not hex: %w", path, err)
	}
	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("%s has %d B, expected %d", path, len(seed), ed25519.SeedSize)
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

func loadState(path string) (state, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return state{}, nil
	}
	if err != nil {
		return state{}, err
	}
	var f stateFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return state{}, fmt.Errorf("%s cannot be read: %w", path, err)
	}
	var out state
	out.Height = f.Height
	b, err := hex.DecodeString(f.BlockID)
	if err != nil || (len(b) != 32 && f.BlockID != "") {
		return state{}, fmt.Errorf("%s has a corrupted block_id", path)
	}
	copy(out.BlockID[:], b)
	return out, nil
}

// saveState writes the state atomically and with an fsync — after a crash there must be no half
// a file left, otherwise the signer forgets what it has already signed.
func saveState(path string, st state) error {
	raw, err := json.Marshal(stateFile{Height: st.Height, BlockID: hex.EncodeToString(st.BlockID[:])})
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(raw, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	// The rename has to reach the disk as well, otherwise the old contents come back after a crash.
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
