package work

import (
	"fmt"
	"strings"
	"testing"

	"powmachina/internal/chain"
	"powmachina/internal/mh"
)

func template(height uint64) *Template {
	return &Template{Height: height, Lane: "G", Bits: fmt.Sprintf("%08x", chain.RegtestTargetBits),
		PrevBlock: strings.Repeat("00", 32), MinTime: 1_700_000_001}
}

// A header carries its height in 32 bits. A template with a height past that would be cut short in
// the header and its epoch would be one nothing can be built for, so no job is made of it.
func TestTemplateWithAHeightThatDoesNotFitAHeaderIsRefused(t *testing.T) {
	payout := make([]byte, 32)

	j, err := Build(template(1<<32-1), payout, chain.RegTest)
	if err != nil {
		t.Fatalf("the last height a header can carry: %v", err)
	}
	if uint64(j.Header.Height) != j.Height || !mh.ValidEpoch(j.Epoch) {
		t.Fatalf("height %d in the job, %d in its header, epoch %d", j.Height, j.Header.Height, j.Epoch)
	}

	for _, height := range []uint64{1 << 32, (1<<42 - 256) * mh.EpochBlocks, 1<<64 - 1} {
		if j, err := Build(template(height), payout, chain.RegTest); err == nil {
			t.Fatalf("height %d gave a job with header height %d and epoch %d", height, j.Header.Height, j.Epoch)
		}
	}
}

// The header carries the version the node asked for: that is where the fork signal bits come
// from. A node that does not send one gets the base version, as before.
func TestJobCarriesTheVersionFromTheTemplate(t *testing.T) {
	payout := make([]byte, 32)

	old := template(10)
	j, err := Build(old, payout, chain.RegTest)
	if err != nil {
		t.Fatal(err)
	}
	if j.Header.Version != chain.VersionBase {
		t.Fatalf("no version in the template: header version %#x, expected %#x", j.Header.Version, chain.VersionBase)
	}

	signalling := template(10)
	signalling.Version = chain.VersionBase | 1<<20
	j, err = Build(signalling, payout, chain.RegTest)
	if err != nil {
		t.Fatal(err)
	}
	if j.Header.Version != signalling.Version {
		t.Fatalf("header version %#x, the template asked for %#x", j.Header.Version, signalling.Version)
	}
	// The version is part of what is hashed, so the PoW input has to carry it too.
	if got := uint32(j.PoW[0]) | uint32(j.PoW[1])<<8 | uint32(j.PoW[2])<<16 | uint32(j.PoW[3])<<24; got != signalling.Version {
		t.Fatalf("the PoW input carries version %#x, expected %#x", got, signalling.Version)
	}
}
