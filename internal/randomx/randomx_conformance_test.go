//go:build mh_stock_salt

// It verifies that **our** build of the vendored RandomX computes the same thing as upstream.
//
// The vectors are copied from `tevador/RandomX @ 615de64`, `src/tests/tests.cpp` (the
// RANDOMX_FLAG_V2 branch). It runs with the stock salt, so we compare directly against upstream:
//
//	go test -tags mh_stock_salt ./internal/randomx/
//
// The test is behind a build tag deliberately — in a normal build the library has the PowMachina salt
// and these vectors would (correctly) not match. It is a conformance test of the **build**, not of
// consensus.
//
// What exactly it guards: the RandomX sources are compiled here as a single unit
// (rx_all_amd64.cpp). If that merging changed anything — a macro from one file leaking into
// another — these three lines would show it.
package randomx

import (
	"encoding/hex"
	"testing"
)

var upstreamVectors = []struct{ input, want string }{
	{"This is a test",
		"22ec6b861b3eb23686b2efbad69513c967ecfce80983df66c9c5b4fbfb4cdb6f"},
	{"Lorem ipsum dolor sit amet",
		"9e2c772c12fd48f93c14c97fdc89d556264d9100597023f44d9163e279012ecf"},
	{"sed do eiusmod tempor incididunt ut labore et dolore magna aliqua",
		"4d6b063a1a603751d525f18a171336a4002f2f06df6c17e4b25fe17e17796e42"},
}

func runUpstream(t *testing.T, mode Mode) {
	t.Helper()
	ctx, err := NewKeyContext([]byte("test key 000"), mode)
	if err != nil {
		t.Fatalf("context: %v", err)
	}
	defer ctx.Close()
	vm, err := ctx.NewVM()
	if err != nil {
		t.Fatalf("vm: %v", err)
	}
	defer vm.Close()

	for _, v := range upstreamVectors {
		got := hex.EncodeToString(mustHash(vm, v.input))
		if got != v.want {
			t.Errorf("%s %q:\n  got   %s\n  want  %s", mode, v.input, got, v.want)
		}
	}
}

func mustHash(vm *VM, s string) []byte {
	h := vm.Hash([]byte(s))
	return h[:]
}

// Light mode (cache only, ~256 MiB) — this is what the node uses to verify a block.
func TestUpstreamVectorsLight(t *testing.T) { runUpstream(t, Light) }

// Fast mode (the full dataset, ~2.1 GiB) — this is what the miner uses. It has to give the same thing.
func TestUpstreamVectorsFast(t *testing.T) {
	if testing.Short() {
		t.Skip("fast mode needs ~2.1 GiB of RAM")
	}
	runUpstream(t, Fast)
}
