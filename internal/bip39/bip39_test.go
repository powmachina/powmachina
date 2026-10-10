package bip39

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
)

// The word list has to be byte for byte the one from BIP39 — the BIP39 README gives its SHA-256.
func TestWordlistMatchesBIP39(t *testing.T) {
	sum := sha256.Sum256([]byte(englishTxt))
	if got := hex.EncodeToString(sum[:]); got != "2f5eed53a4727b4bf8880d8f3f199efc90e58503646d9ff8eff3a2ed3b24dbda" {
		t.Fatalf("english.txt has SHA-256 %s — this is not the BIP39 list", got)
	}
}

// The official vectors (Trezor python-mnemonic, passphrase "TREZOR").
func TestTrezorVectors(t *testing.T) {
	data, err := os.ReadFile("testdata/trezor-english.json")
	if err != nil {
		t.Fatal(err)
	}
	var vectors [][]string
	if err := json.Unmarshal(data, &vectors); err != nil {
		t.Fatal(err)
	}
	if len(vectors) < 24 {
		t.Fatalf("there are only %d vectors", len(vectors))
	}
	for i, v := range vectors {
		ent, _ := hex.DecodeString(v[0])
		m, err := NewMnemonic(ent)
		if err != nil {
			t.Fatal(err)
		}
		if m != v[1] {
			t.Fatalf("vector %d: mnemonic\n %s\nwant\n %s", i, m, v[1])
		}
		back, err := Entropy(m)
		if err != nil || hex.EncodeToString(back) != v[0] {
			t.Fatalf("vector %d: back to entropy %x, %v", i, back, err)
		}
		seed, err := Seed(m, "TREZOR")
		if err != nil {
			t.Fatal(err)
		}
		if hex.EncodeToString(seed) != v[2] {
			t.Fatalf("vector %d: the seed does not match", i)
		}
	}
}

func TestChecksumCatchesTypoAndSwap(t *testing.T) {
	m, _ := NewMnemonic(make([]byte, 32)) // "abandon … art"
	ws := strings.Fields(m)
	ws[0], ws[23] = ws[23], ws[0]
	if _, err := Entropy(strings.Join(ws, " ")); !errors.Is(err, ErrChecksum) {
		t.Fatalf("swapped words: %v", err)
	}
	if _, err := Entropy(strings.Replace(m, "abandon", "abandonx", 1)); err == nil {
		t.Fatal("an unknown word passed")
	}
}

// A person may type the first four letters and upper case; the result is the same seed.
func TestNormalizeAcceptsPrefixes(t *testing.T) {
	m, _ := NewMnemonic([]byte("0123456789abcdef0123456789abcdef"))
	var short []string
	for _, w := range strings.Fields(m) {
		if len(w) > 4 {
			w = w[:4]
		}
		short = append(short, strings.ToUpper(w))
	}
	n, err := Normalize("  " + strings.Join(short, "   ") + "\n")
	if err != nil || n != m {
		t.Fatalf("shortened words: %q, %v", n, err)
	}
}

func TestSeedRejectsNonASCIIPassphrase(t *testing.T) {
	m, _ := NewMnemonic(make([]byte, 16))
	if _, err := Seed(m, "passphrase-é"); !errors.Is(err, ErrNonASCIIPass) {
		t.Fatalf("a non-ASCII passphrase: %v", err)
	}
}
