package p2p

import (
	"crypto/ed25519"
	"testing"

	"powmachina/internal/chain"
)

// Both the request and the answer have to round-trip, and the caps have to hold — otherwise a request
// is the cheapest way to force a large answer.
func TestGetAttestRoundTripAndLimits(t *testing.T) {
	raw, err := EncodeGetAttest(100, 20)
	if err != nil {
		t.Fatal(err)
	}
	from, count, err := DecodeGetAttest(raw)
	if err != nil || from != 100 || count != 20 {
		t.Fatalf("round-trip: %d, %d, %v", from, count, err)
	}
	if _, err := EncodeGetAttest(1, 0); err == nil {
		t.Fatal("a zero count of heights should have been refused")
	}
	if _, err := EncodeGetAttest(1, MaxAttestHeights+1); err == nil {
		t.Fatal("above the height cap it should have been refused")
	}
	// A foreign peer can send anything.
	if _, _, err := DecodeGetAttest([]byte{1, 2, 3}); err == nil {
		t.Fatal("a short payload should have been refused")
	}
	bad, _ := EncodeGetAttest(1, 2)
	bad[8], bad[9] = 0xff, 0xff // a count of 65535
	if _, _, err := DecodeGetAttest(bad); err == nil {
		t.Fatal("a count above the cap should have been refused")
	}
}

func TestAttestationsRoundTrip(t *testing.T) {
	var list []chain.Attestation
	for i := 0; i < 5; i++ {
		seed := make([]byte, ed25519.SeedSize)
		seed[0] = byte(i)
		list = append(list, chain.Sign(ed25519.NewKeyFromSeed(seed), uint64(100+i), [32]byte{byte(i)}))
	}
	raw, err := EncodeAttestations(list)
	if err != nil {
		t.Fatal(err)
	}
	back, err := DecodeAttestations(raw)
	if err != nil || len(back) != len(list) {
		t.Fatalf("round-trip: %d attestations, %v", len(back), err)
	}
	for i := range list {
		if back[i].Height != list[i].Height || back[i].Key != list[i].Key ||
			back[i].BlockID != list[i].BlockID || string(back[i].Signature) != string(list[i].Signature) {
			t.Fatalf("attestation %d does not match", i)
		}
	}

	// An empty answer is valid — "I have nothing".
	empty, err := EncodeAttestations(nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := DecodeAttestations(empty); err != nil || len(got) != 0 {
		t.Fatalf("an empty answer: %d, %v", len(got), err)
	}

	// The count cap and a corrupted length.
	if _, err := EncodeAttestations(make([]chain.Attestation, MaxAttestPerMessage+1)); err == nil {
		t.Fatal("above the attestation cap it should have been refused")
	}
	if _, err := DecodeAttestations(append(raw, 0)); err == nil {
		t.Fatal("a length that does not match the count should have been refused")
	}
}
