package chain

import (
	"encoding/hex"
	"testing"

	"powmachina/internal/mh"
)

// The genesis is the root of trust. If its ID changed, a node with another binary would silently
// run on a different network — and it would only show up as peers not understanding each other.
// Hence a golden test.
func TestGenesisIDsAreFixed(t *testing.T) {
	want := map[string]string{
		"testnet": "59f614c9c994a653550809da0aa02e4637eb9911f5eb514e662931a9e68eba55",
		// The regtest genesis depends on RegtestTargetBits (the bits are part of the header). A
		// datadir made with different bits is refused as "a datadir from another network", which
		// is exactly what this test protects.
		"regtest": "03c4ab1dc7087a26131c1ca472f2eefa33f7d95f03233c9d5b8489122c48c2fc",
	}
	got := map[string]string{
		"testnet": hex.EncodeToString(idOf(TestnetGenesis())),
		"regtest": hex.EncodeToString(idOf(RegtestGenesis())),
	}
	for name, w := range want {
		t.Logf("%s genesis = %s", name, got[name])
		if w != "" && got[name] != w {
			t.Fatalf("%s genesis %s, expected %s", name, got[name], w)
		}
	}
	if got["testnet"] == got["regtest"] {
		t.Fatal("testnet and regtest have the same genesis")
	}
	if TestNet.Magic == RegTest.Magic || TestNet.Magic == MainNet.Magic {
		t.Fatal("the networks' magic numbers overlap")
	}
}

func idOf(h *Header) []byte {
	id := h.ID()
	return id[:]
}

// Genesis(params) has to return what the network really uses — and nothing for mainnet, so a
// placeholder cannot be used by mistake as a real network's root (SPEC §5).
func TestGenesisForNetwork(t *testing.T) {
	if got := Genesis(TestNet); got == nil || got.ID() != TestnetGenesis().ID() {
		t.Fatal("Genesis(TestNet) did not return the testnet genesis")
	}
	if got := Genesis(RegTest); got == nil || got.ID() != RegtestGenesis().ID() {
		t.Fatal("Genesis(RegTest) did not return the regtest genesis")
	}
	if Genesis(MainNet) != nil {
		t.Fatal("the mainnet genesis should not exist yet")
	}
}

// Testnet runs on the mainnet parameters — that is its whole point. Regtest does not.
func TestTestnetUsesMainnetParams(t *testing.T) {
	if TestNet.PowLimitBits != MainNet.PowLimitBits {
		t.Fatalf("testnet PowLimit %08x, mainnet %08x", TestNet.PowLimitBits, MainNet.PowLimitBits)
	}
	if TestNet.DagProfile != (mh.DagProfile{}) {
		t.Fatalf("testnet has its own DAG profile %+v, expected the mainnet one", TestNet.DagProfile)
	}
	if TestnetGenesis().Bits != MainNet.PowLimitBits {
		t.Fatalf("the testnet genesis has bits %08x", TestnetGenesis().Bits)
	}
}
