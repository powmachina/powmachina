package address

import (
	"strings"
	"testing"
)

func TestSecretRoundTripAndSeparation(t *testing.T) {
	var k [32]byte
	for i := range k {
		k[i] = byte(i + 1)
	}
	s, err := EncodeSecret("powm", &k)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(s, "powmsec1") {
		t.Fatalf("the key %s does not have the powmsec1 prefix", s)
	}
	got, err := DecodeSecret("powm", s)
	if err != nil || *got != k {
		t.Fatalf("round trip: %x, %v", got, err)
	}
	// A different network, an address instead of a key, a typo — reject all of it.
	if _, err := DecodeSecret("tpowm", s); err == nil {
		t.Fatal("a mainnet key passed on testnet")
	}
	if _, _, err := Decode("powm", s); err == nil {
		t.Fatal("a private key passed as an address")
	}
	addr, _ := Encode("powm", 0, k[:])
	if _, err := DecodeSecret("powm", addr); err == nil {
		t.Fatal("an address passed as a private key")
	}
	typo := []byte(s)
	typo[12] ^= 1
	if _, err := DecodeSecret("powm", string(typo)); err == nil {
		t.Fatal("a typo in the key passed")
	}
}
