package address

import (
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

// Test vectors from BIP350. If our implementation differed from the standard, nobody would be able to
// use the existing tooling with PowMachina.
//
// The strongest of them is TestMatchesSegwitVector below: a canonical P2TR address exercises the polymod,
// the bit conversion and the charset at once. One vector from the list is deliberately missing here — it
// could not be verified reliably, and adapting the test to our own output would have been circular.
func TestBIP350ValidStrings(t *testing.T) {
	valid := []string{
		"A1LQFN3A",
		"a1lqfn3a",
		"an83characterlonghumanreadablepartthatcontainsthetheexcludedcharactersbioandnumber11sg7hg6",
		"11llllllllllllllllllllllllllllllllllllllllllllllllllllllllllllllllllllllllllllllllllludsr8",
		"split1checkupstagehandshakeupstreamerranterredcaperredlc445v",
		"?1v759aa",
	}
	for _, s := range valid {
		if _, _, err := DecodeBech32m(s); err != nil {
			t.Errorf("BIP350 vector %q should have passed: %v", s, err)
		}
	}
}

func TestBIP350InvalidStrings(t *testing.T) {
	invalid := map[string]string{
		"\x201xj0phk": "an HRP character below the range",
		"\x7f1g6xzxy": "an HRP character above the range",
		"an84characterslonghumanreadablepartthatcontainsthetheexcludedcharactersbioandnumber11d6pts4": "too long",
		"qyrz8wqd2c9m":  "the separator is missing",
		"1qyrz8wqd2c9m": "an empty HRP",
		"y1b0jsk6g":     "an invalid character in the data",
		"lt1igcx5c0":    "an invalid character in the data",
		"in1muywd":      "the checksum is too short",
		"mm1crxm3i":     "an invalid character in the checksum",
		"au1s5cgom":     "an invalid character in the checksum",
		"M1VUXWEZ":      "a wrong checksum",
		"16plkw9":       "empty data",
		"1p2gdwpf":      "an empty HRP",
		"bc1q9zpgru":    "a bech32 (not bech32m) checksum",
	}
	for s, why := range invalid {
		if _, _, err := DecodeBech32m(s); err == nil {
			t.Errorf("%q should have failed (%s)", s, why)
		}
	}
}

// A P2TR-style address from BIP350 — proof that our encoder gives the same results as the reference one.
func TestMatchesSegwitVector(t *testing.T) {
	program, _ := hex.DecodeString("79be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798")
	got, err := Encode("bc", 1, program)
	if err != nil {
		t.Fatal(err)
	}
	want := "bc1p0xlxvlhemja6c4dqv22uapctqupfhlxm9h8z3k2e72q4k9hcz7vqzk5jj0"
	if got != want {
		t.Fatalf("the encoder differs from BIP350:\n got:  %s\n want: %s", got, want)
	}
	version, decoded, err := Decode("bc", want)
	if err != nil {
		t.Fatal(err)
	}
	if version != 1 || hex.EncodeToString(decoded) != hex.EncodeToString(program) {
		t.Fatal("the decoder did not return the original program")
	}
}

// This is why bech32m exists: a single typo has to invalidate an address.
func TestSingleTypoIsCaught(t *testing.T) {
	program := make([]byte, 32)
	for i := range program {
		program[i] = byte(i)
	}
	addr, err := Encode("rpowm", 0, program)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := Decode("rpowm", addr); err != nil {
		t.Fatalf("the original address has to pass: %v", err)
	}

	caught := 0
	for i := len("rpowm1"); i < len(addr); i++ {
		for _, c := range charset {
			if byte(c) == addr[i] {
				continue
			}
			typo := addr[:i] + string(c) + addr[i+1:]
			if _, _, err := Decode("rpowm", typo); err == nil {
				t.Fatalf("a typo at position %d (%c -> %c) went undetected", i, addr[i], c)
			}
			caught++
		}
	}
	t.Logf("detected %d single-character typos", caught)
}

// An address from one network must not be valid on another — otherwise mainnet money could be sent to a
// testnet address.
func TestNetworkMismatchRejected(t *testing.T) {
	program := make([]byte, 32)
	addr, err := Encode("powm", 0, program)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := Decode("tpowm", addr); !errors.Is(err, ErrBadHRP) {
		t.Fatalf("an address from another network has to fail, got %v", err)
	}
}

func TestMixedCaseAndLimits(t *testing.T) {
	program := make([]byte, 32)
	addr, _ := Encode("rpowm", 0, program)

	mixed := strings.ToUpper(addr[:8]) + addr[8:]
	if _, _, err := Decode("rpowm", mixed); !errors.Is(err, ErrMixedCase) {
		t.Fatalf("mixed case has to fail, got %v", err)
	}
	// Upper case on its own is fine.
	if _, _, err := Decode("rpowm", strings.ToUpper(addr)); err != nil {
		t.Fatalf("a fully upper-case address has to pass: %v", err)
	}
	if _, err := Encode("rpowm", 16, program); err == nil {
		t.Fatal("a version above 15 must not be encodable")
	}
	if _, err := Encode("rpowm", 0, make([]byte, 41)); err == nil {
		t.Fatal("a program over 40 bytes must not be encodable")
	}
}

// An address from a user is untrusted input.
func FuzzDecodeAddress(f *testing.F) {
	program := make([]byte, 32)
	addr, _ := Encode("rpowm", 0, program)
	f.Add(addr)
	f.Add("")
	f.Add("rpowm1")
	f.Add(strings.Repeat("q", 200))

	f.Fuzz(func(t *testing.T, s string) {
		version, prog, err := Decode("rpowm", s)
		if err != nil {
			return
		}
		if version > 15 {
			t.Fatalf("accepted version %d", version)
		}
		if len(prog) < 2 || len(prog) > 40 {
			t.Fatalf("accepted a program of length %d", len(prog))
		}
		// Whatever passed must encode back to the same string.
		back, err := Encode("rpowm", version, prog)
		if err != nil {
			t.Fatalf("an accepted address cannot be encoded back: %v", err)
		}
		if !strings.EqualFold(back, s) {
			t.Fatalf("the round trip changed the address:\n %s\n %s", s, back)
		}
	})
}
