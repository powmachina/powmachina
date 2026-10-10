package hd

import (
	"encoding/hex"
	"errors"
	"testing"
)

// Test vector 1 from BIP32. If it matches, HMAC, the byte order and the curve arithmetic all do exactly
// what the standard says — and a seed from our wallet will open elsewhere.
func TestBIP32Vector1(t *testing.T) {
	seed, _ := hex.DecodeString("000102030405060708090a0b0c0d0e0f")
	master, err := NewMaster(seed)
	if err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(master.Priv[:]); got != "e8f32e723decf4051aefac8e2c93c9c5b214313817cdb01a1494b917c8436b35" {
		t.Errorf("the master private key does not match BIP32:\n got:   %s", got)
	}
	if got := hex.EncodeToString(master.ChainCode[:]); got != "873dff81c02f525623fd1fe5167eac3a55a049de3d314bb42ee227ffed37d508" {
		t.Errorf("the chain code does not match BIP32:\n got:   %s", got)
	}

	// m/0'
	child, err := master.Derive("m/0'")
	if err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(child.Priv[:]); got != "edb2e14f9ee77d26dd93b4ecede8d16ed408ce149b6cd80b0715a2d911a0afea" {
		t.Errorf("m/0' does not match BIP32:\n got:   %s", got)
	}
	if child.Depth != 1 || child.ChildNumber != HardenedOffset {
		t.Errorf("the child's depth or index does not match: %d, %d", child.Depth, child.ChildNumber)
	}
}

// The same seed must always give the same keys — the whole idea of a backup rests on that.
func TestDerivationIsDeterministic(t *testing.T) {
	seed := []byte("PowMachina test seed 32 bytes ok")
	a, err := NewMaster(seed)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := NewMaster(seed)

	k1, _ := a.Derive("m/44'/9797'/0'/0/7")
	k2, _ := b.Derive("m/44'/9797'/0'/0/7")
	if k1.Priv != k2.Priv {
		t.Fatal("the same seed and path gave a different key")
	}

	// A different index must give a different key.
	k3, _ := a.Derive("m/44'/9797'/0'/0/8")
	if k1.Priv == k3.Priv {
		t.Fatal("a different index gave the same key")
	}
	// A hardened and a non-hardened branch have to diverge.
	h, _ := a.Derive("m/44'/9797'/0'/0/7'")
	if h.Priv == k1.Priv {
		t.Fatal("a hardened and a non-hardened index gave the same key")
	}
}

func TestPathParsing(t *testing.T) {
	seed := []byte("PowMachina test seed 32 bytes ok")
	m, err := NewMaster(seed)
	if err != nil {
		t.Fatal(err)
	}
	// The three ways of writing hardening are equivalent.
	a, _ := m.Derive("m/44'")
	b, _ := m.Derive("m/44h")
	c, _ := m.Derive("m/44H")
	if a.Priv != b.Priv || a.Priv != c.Priv {
		t.Fatal("the ways of writing hardening do not agree")
	}
	// A bare m is the master key.
	root, _ := m.Derive("m")
	if root.Priv != m.Priv {
		t.Fatal("the path \"m\" has to return the master key")
	}

	for _, bad := range []string{"", "44'", "x/44", "m/", "m/44''", "m/-1", "m/2147483648", "m/abc"} {
		if _, err := m.Derive(bad); !errors.Is(err, ErrBadPath) && err == nil {
			t.Errorf("the path %q should have been rejected", bad)
		}
	}
}

func TestSeedValidation(t *testing.T) {
	if _, err := NewMaster(make([]byte, 15)); !errors.Is(err, ErrShortSeed) {
		t.Fatal("a short seed has to be rejected")
	}
	if _, err := NewMaster(make([]byte, 16)); err != nil {
		t.Fatalf("a 16-byte seed has to pass: %v", err)
	}
}

// Every derived key must yield a usable address.
func TestDerivedKeysProduceAddresses(t *testing.T) {
	seed := []byte("PowMachina test seed 32 bytes ok")
	m, _ := NewMaster(seed)
	seen := make(map[[32]byte]bool)
	for i := 0; i < 20; i++ {
		k, err := m.Derive("m/44'/9797'/0'/0/" + string(rune('0'+i%10)))
		if err != nil {
			t.Fatal(err)
		}
		pk, err := k.XOnlyPubkey()
		if err != nil {
			t.Fatal(err)
		}
		if pk == [32]byte{} {
			t.Fatal("an empty public key")
		}
		seen[pk] = true
	}
	if len(seen) < 10 {
		t.Fatalf("expected at least 10 distinct keys, got %d", len(seen))
	}
}
