package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"powmachina/internal/chain"
	"powmachina/internal/p2p"
)

// fakeNode is a node that remembers what the signer sent it.
type fakeNode struct {
	height    uint64
	ids       map[uint64][32]byte
	sent      []chain.Attestation
	failed    bool
	quorum    bool   // an accepted attestation advances finality (a node with a full quorum)
	finalized uint64 // 0 = the node has nothing finalized
}

func (f *fakeNode) Finality(context.Context) (uint64, bool, error) {
	return f.finalized, f.finalized > 0, nil
}

func (f *fakeNode) Tip() (uint64, [32]byte, error) { return f.height, f.ids[f.height], nil }

func (f *fakeNode) BlockHashAt(_ context.Context, h uint64) ([32]byte, error) {
	id, ok := f.ids[h]
	if !ok {
		return [32]byte{}, errors.New("we do not have that height")
	}
	return id, nil
}

func (f *fakeNode) SubmitAttestation(_ context.Context, attHex string) (string, error) {
	if f.failed {
		return "", errors.New("the node did not answer")
	}
	raw, err := hex.DecodeString(attHex)
	if err != nil {
		return "", err
	}
	att, err := p2p.DecodeAttestation(raw)
	if err != nil {
		return "", err
	}
	f.sent = append(f.sent, att)
	if f.quorum {
		f.finalized = att.Height // a node with a working quorum advances finality
	}
	return "", nil
}

func testSigner(t *testing.T, node nodeRPC) (*signer, ed25519.PrivateKey) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return &signer{params: chain.RegTest, index: 0, priv: priv, client: node,
		statePath: filepath.Join(t.TempDir(), "signer.state")}, priv
}

func chainOf(n uint64) map[uint64][32]byte {
	out := make(map[uint64][32]byte, n+1)
	for h := uint64(0); h <= n; h++ {
		var id [32]byte
		id[0], id[1] = byte(h), byte(h>>8)
		out[h] = id
	}
	return out
}

// The signer signs the block at the finality depth — and never signs the same height twice.
// Double signing is the one thing a quorum must not do: there is a proof for it that turns
// finality off for that height across the whole network.
func TestSignerSignsEachHeightOnce(t *testing.T) {
	node := &fakeNode{height: chain.FinalityDepth + 3, ids: chainOf(chain.FinalityDepth + 3),
		quorum: true}
	s, priv := testSigner(t, node)
	ctx := context.Background()

	s.once(ctx)
	if len(node.sent) != 1 {
		t.Fatalf("expected one attestation, got %d", len(node.sent))
	}
	want := node.height - chain.FinalityDepth
	att := node.sent[0]
	if att.Height != want {
		t.Fatalf("signed height %d, expected %d", att.Height, want)
	}
	if !ed25519.Verify(priv.Public().(ed25519.PublicKey),
		chain.AttestationMessage(att.Height, att.BlockID), att.Signature) {
		t.Fatal("the signature is not valid")
	}

	// Further rounds without a new block must send nothing.
	s.once(ctx)
	s.once(ctx)
	if len(node.sent) != 1 {
		t.Fatalf("the same height was signed %d times", len(node.sent))
	}

	// A new block moves the depth by one — the next height is signed.
	node.height++
	node.ids = chainOf(node.height)
	s.once(ctx)
	if len(node.sent) != 2 || node.sent[1].Height != want+1 {
		t.Fatalf("after a new block expected height %d, got %+v", want+1, node.sent)
	}
}

// The state is written BEFORE sending. When the node does not accept an attestation, the signer
// may send **the same** attestation again (Ed25519 is deterministic, so it is the same
// signature), but it must never sign a different block for that height — there is a proof for
// that which turns finality off.
func TestSignerRetriesSameAttestationAfterFailedSubmit(t *testing.T) {
	node := &fakeNode{height: chain.FinalityDepth + 1, ids: chainOf(chain.FinalityDepth + 1),
		failed: true}
	s, _ := testSigner(t, node)
	ctx := context.Background()

	s.once(ctx)
	if len(node.sent) != 0 {
		t.Fatal("the attestation was sent even though the node failed")
	}
	target := node.height - chain.FinalityDepth
	st, err := loadState(s.statePath)
	if err != nil {
		t.Fatal(err)
	}
	if st.Height != target {
		t.Fatalf("the state was not written before sending: %+v", st)
	}

	// The node recovered: the same attestation is sent, not a new height.
	node.failed = false
	s.once(ctx)
	if len(node.sent) != 1 {
		t.Fatalf("the attestation was not sent again after the failure (%d)", len(node.sent))
	}
	if got := node.sent[0]; got.Height != target || got.BlockID != node.ids[target] {
		t.Fatalf("a different attestation was sent: %d/%x", got.Height, got.BlockID[:4])
	}
	// And never two different attestations for the same height.
	for i := 0; i < 3; i++ {
		s.once(ctx)
	}
	for _, a := range node.sent {
		if a.Height == target && a.BlockID != node.ids[target] {
			t.Fatalf("a different block was signed at height %d: %x", target, a.BlockID[:4])
		}
	}
}

// The state has to survive a process restart — after it, a height that is already signed must not be signed again.
func TestSignerStateSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "signer.state")
	node := &fakeNode{height: chain.FinalityDepth + 5, ids: chainOf(chain.FinalityDepth + 5),
		quorum: true}

	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	first := &signer{params: chain.RegTest, index: 0, priv: priv, client: node, statePath: path}
	first.once(context.Background())
	if len(node.sent) != 1 {
		t.Fatalf("expected one attestation, got %d", len(node.sent))
	}

	last, err := loadState(path)
	if err != nil {
		t.Fatal(err)
	}
	second := &signer{params: chain.RegTest, index: 0, priv: priv, client: node,
		statePath: path, last: last}
	second.once(context.Background())
	if len(node.sent) != 1 {
		t.Fatalf("the height was signed again after the restart (%d attestations)", len(node.sent))
	}
}

// A chain shorter than the finality depth: there is nothing to sign and it must not crash either.
func TestSignerWaitsForEnoughBlocks(t *testing.T) {
	node := &fakeNode{height: chain.FinalityDepth - 1, ids: chainOf(chain.FinalityDepth - 1)}
	s, _ := testSigner(t, node)
	s.once(context.Background())
	if len(node.sent) != 0 {
		t.Fatalf("it signed too early: %+v", node.sent)
	}
}

// A key that does not belong to the index has to be refused by the program at startup —
// otherwise it would send attestations the quorum drops and finality would just silently not advance.
func TestCheckKeyMatchesIndex(t *testing.T) {
	if err := checkKeyMatchesIndex(chain.RegTest, 2, chain.RegtestSignerKey(2)); err != nil {
		t.Fatalf("the correct key was refused: %v", err)
	}
	if err := checkKeyMatchesIndex(chain.RegTest, 2, chain.RegtestSignerKey(3)); err == nil {
		t.Fatal("a foreign key should have been refused")
	}
	if err := checkKeyMatchesIndex(chain.RegTest, 99, chain.RegtestSignerKey(0)); err == nil {
		t.Fatal("an index outside the quorum should have been refused")
	}
	if err := checkKeyMatchesIndex(chain.TestNet, 0, chain.RegtestSignerKey(0)); err == nil {
		t.Fatal("a regtest key does not belong to the testnet quorum")
	}
}

// A quorum key has to be readable by its owner only and it has to be a hex seed.
func TestLoadKeyChecksPermissionsAndFormat(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "ok.key")
	seed := strings.Repeat("ab", ed25519.SeedSize)
	if err := os.WriteFile(good, []byte(seed+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadKey(good); err != nil {
		t.Fatalf("the correct key failed: %v", err)
	}

	open := filepath.Join(dir, "open.key")
	if err := os.WriteFile(open, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadKey(open); err == nil {
		t.Fatal("a key with 0644 permissions should have been refused")
	}

	short := filepath.Join(dir, "short.key")
	if err := os.WriteFile(short, []byte("aabb"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadKey(short); err == nil {
		t.Fatal("a short seed should have been refused")
	}

	junk := filepath.Join(dir, "junk.key")
	if err := os.WriteFile(junk, []byte("this is not hex"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadKey(junk); err == nil {
		t.Fatal("a non-hex key should have been refused")
	}
}

// When the whole network restarts, the nodes lose the attestations (finality state lives only in
// memory). The signer has to send its last signed attestation again — otherwise finality would
// stand still until the chain grew by another FinalityDepth blocks.
//
// What is repeated is **the same** attestation, not a second signature: Ed25519 is deterministic.
func TestSignerResendsUntilFinalityCatchesUp(t *testing.T) {
	node := &fakeNode{height: chain.FinalityDepth + 7, ids: chainOf(chain.FinalityDepth + 7)}
	s, _ := testSigner(t, node)
	ctx := context.Background()

	s.once(ctx)
	if len(node.sent) != 1 {
		t.Fatalf("expected one attestation, got %d", len(node.sent))
	}
	signed := node.sent[0]

	// The node has no finality (it restarted) → the same height goes again.
	s.once(ctx)
	if len(node.sent) != 2 {
		t.Fatalf("the attestation was not sent again (%d)", len(node.sent))
	}
	again := node.sent[1]
	if again.Height != signed.Height || again.BlockID != signed.BlockID {
		t.Fatalf("a different attestation was sent: %d/%x vs %d/%x",
			again.Height, again.BlockID[:4], signed.Height, signed.BlockID[:4])
	}
	if string(again.Signature) != string(signed.Signature) {
		t.Fatal("a repeat has to be the same signature, not a new one")
	}

	// Once finality reaches that height, the repeating stops.
	node.finalized = signed.Height
	s.once(ctx)
	s.once(ctx)
	if len(node.sent) != 2 {
		t.Fatalf("the attestation was still being sent after finality was reached (%d)", len(node.sent))
	}
}
