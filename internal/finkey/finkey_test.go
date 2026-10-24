package finkey

import (
	"os"
	"path/filepath"
	"testing"

	"powmachina/internal/chain"
)

func load(t *testing.T, dir string) *Signer {
	t.Helper()
	s, _, err := Load(filepath.Join(dir, "finality.key"), filepath.Join(dir, "finality.signed"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func sign(s *Signer, h uint64, id [32]byte) (chain.Attestation, bool) {
	a, err := s.Sign(h, id)
	return a, err == nil
}

// A signer never signs two blocks at one height, nor anything below what it signed —
// not even after a restart, which is why the record is on disk before the signature exists.
func TestTheSignerNeverSignsTwice(t *testing.T) {
	dir := t.TempDir()
	s := load(t, dir)
	a, ok := sign(s, 100, [32]byte{1})
	if !ok {
		t.Fatal("the first signature")
	}
	if _, ok := sign(s, 100, [32]byte{2}); ok {
		t.Fatal("a second block at the same height was signed")
	}
	if _, ok := sign(s, 99, [32]byte{3}); ok {
		t.Fatal("a height below the last one was signed")
	}
	again, ok := sign(s, 100, [32]byte{1})
	if !ok || string(again.Signature) != string(a.Signature) {
		t.Fatal("the same block again should give the same signature")
	}

	reopened := load(t, dir)
	if reopened.Pub != s.Pub {
		t.Fatal("the key changed across a restart")
	}
	if _, ok := sign(reopened, 100, [32]byte{2}); ok {
		t.Fatal("after a restart a second block at the same height was signed")
	}
	if _, ok := sign(reopened, 101, [32]byte{4}); !ok {
		t.Fatal("the next height was refused")
	}
	if fi, err := os.Stat(filepath.Join(dir, "finality.key")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("the key file: %v %v", fi.Mode(), err)
	}
}
