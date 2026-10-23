package mh

import (
	"bufio"
	"encoding/hex"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"
)

// This is the most important test of the whole node: it proves that the algorithm gives bit for bit
// the same thing it gave on the day the vectors came about. If they diverged, the node would reject
// its own miners' blocks — and the GPU kernels, validated against the same file, would be on a
// different network.
//
// The vectors are the contract, not the code: any reimplementation of the core has to reproduce
// them **unchanged**. That is exactly what they are for.
func TestStoredVectorsMatch(t *testing.T) {
	f, err := os.Open("../../vectors/mh-test-vectors.txt")
	if err != nil {
		t.Fatalf("the test vectors are missing: %v", err)
	}
	defer f.Close()

	v := NewVerifier(0, TestProfile)
	defer v.Close()
	// The vectors contain both lanes, so the hasher needs RandomX too. Light mode is enough — it has
	// to give the same results as fast (verified by internal/randomx/randomx_conformance_test.go).
	rx, err := NewRandomX(0, false)
	if err != nil {
		t.Skipf("RandomX cannot be initialized: %v", err)
	}
	defer rx.Close()
	h, err := NewHasherWithRandomX(rx)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	lanes := map[string]Lane{"C": LaneC, "G": LaneG}
	checked := 0

	sc := bufio.NewScanner(f)
	for line := 1; sc.Scan(); line++ {
		text := sc.Text()
		if strings.HasPrefix(text, "#") || strings.TrimSpace(text) == "" {
			continue
		}
		parts := strings.Fields(text)
		if len(parts) != 4 {
			t.Fatalf("line %d: expected 4 fields, got %d", line, len(parts))
		}
		lane, ok := lanes[parts[0]]
		if !ok {
			t.Fatalf("line %d: unknown lane %q", line, parts[0])
		}
		raw, err := hex.DecodeString(parts[1])
		if err != nil || len(raw) != HeaderBytes {
			t.Fatalf("line %d: a bad header: %v", line, err)
		}
		nonce, err := strconv.ParseUint(parts[2], 10, 64)
		if err != nil {
			t.Fatalf("line %d: a bad nonce: %v", line, err)
		}

		var header [HeaderBytes]byte
		copy(header[:], raw)

		got, err := h.Hash(v, &header, nonce, lane)
		if err != nil {
			t.Fatalf("line %d: %v", line, err)
		}
		if want := parts[3]; hex.EncodeToString(got[:]) != want {
			t.Fatalf("line %d (lane %s, nonce %d):\n computed: %s\n in the vector: %s",
				line, lane, nonce, hex.EncodeToString(got[:]), want)
		}
		checked++
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if checked < 100 {
		t.Fatalf("only %d vectors were verified", checked)
	}
	t.Logf("%d vectors verified", checked)
}

func TestMeetsTarget(t *testing.T) {
	var hash, target [32]byte
	hash[1], target[1] = 0x10, 0x20
	if !MeetsTarget(&hash, &target) {
		t.Fatal("a smaller hash has to meet the target")
	}
	if MeetsTarget(&target, &hash) {
		t.Fatal("a larger hash must not meet the target")
	}
	if !MeetsTarget(&hash, &hash) {
		t.Fatal("equality counts as meeting it")
	}
}

// A hasher without a RandomX context has to refuse lane C, not silently compute something else.
func TestLaneCNeedsRandomX(t *testing.T) {
	v := NewVerifier(0, TestProfile)
	defer v.Close()
	h := NewHasher()
	defer h.Close()

	var header [HeaderBytes]byte
	if _, err := h.Hash(v, &header, 1, LaneC); !errors.Is(err, ErrNoRandomX) {
		t.Fatalf("expected ErrNoRandomX, got %v", err)
	}
	// Lane G has to work without it too — a node verifying only the GPU lane does not need RandomX.
	if _, err := h.Hash(v, &header, 1, LaneG); err != nil {
		t.Fatalf("lane G should not need RandomX: %v", err)
	}
}

// The epoch is consensus — it applies to both lane G's DAG and lane C's RandomX key.
func TestEpochForHeight(t *testing.T) {
	if EpochForHeight(0) != 0 {
		t.Fatal("height 0 should be epoch 0")
	}
	if EpochForHeight(20_000) != 1 || EpochForHeight(19_999) != 0 {
		t.Fatalf("the epoch boundary does not match: %d %d", EpochForHeight(19_999), EpochForHeight(20_000))
	}
}
