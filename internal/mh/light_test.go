package mh

import "testing"

// A light verifier has to give a bit-identical hash to the full one — otherwise a light node would
// reject valid blocks (or accept invalid ones) and split off the network.
func TestLightVerifierMatchesFull(t *testing.T) {
	full := NewVerifier(3, TestProfile)
	light := NewLightVerifier(3, TestProfile)
	lightened := full.Lightened()
	filled := light.Filled()
	if full.Light() || !light.Light() || !lightened.Light() || filled.Light() {
		t.Fatal("the verifiers' modes do not match")
	}
	h := NewHasher()
	var header [HeaderBytes]byte
	for i := range header {
		header[i] = byte(i * 7)
	}
	for nonce := uint64(0); nonce < 64; nonce++ {
		want, err := h.Hash(full, &header, nonce, LaneG)
		if err != nil {
			t.Fatal(err)
		}
		for name, v := range map[string]*Verifier{"light": light, "lightened": lightened, "filled": filled} {
			got, err := h.Hash(v, &header, nonce, LaneG)
			if err != nil {
				t.Fatal(err)
			}
			if got != want {
				t.Fatalf("nonce %d: the %s verifier gave a different hash than the full one", nonce, name)
			}
		}
	}
}

// Lightened must not break the original full verifier — the node swaps it and closes the old one only afterwards.
func TestLightenedKeepsOriginal(t *testing.T) {
	full := NewVerifier(0, TestProfile)
	_ = full.Lightened()
	if full.Light() || full.Dag() == nil {
		t.Fatal("Lightened changed the original verifier")
	}
}

// Pipelined lane C hashing (the miner) has to give exactly the same thing as HashLaneC (the node).
func TestLaneCPipeMatchesHash(t *testing.T) {
	rx, err := NewRandomX(0, false)
	if err != nil {
		t.Skipf("RandomX: %v", err)
	}
	defer rx.Close()
	h, err := NewHasherWithRandomX(rx)
	if err != nil {
		t.Fatal(err)
	}
	var hdr [HeaderBytes]byte
	for i := range hdr {
		hdr[i] = byte(i * 3)
	}
	p, err := h.PipeLaneC(&hdr)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := NewHasherWithRandomX(rx) // the reference on a VM of its own
	if err != nil {
		t.Fatal(err)
	}
	p.Start(100)
	for n := uint64(101); n < 130; n++ { // a contiguous chain without a restart
		var got [32]byte
		p.Next(n, &got)
		want, _ := ref.HashLaneC(&hdr, n-1)
		if got != want {
			t.Fatalf("nonce %d: the pipelined hash differs", n-1)
		}
	}
}
