package address

import (
	"strings"
	"testing"

	"powmachina/internal/chain"
)

// The prefix is part of the coin's identity and is visible on every address. This test locks it down:
// after genesis, changing it would mean existing wallets stop recognizing addresses.
func TestNetworkPrefixesAreLocked(t *testing.T) {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	cases := []struct {
		params chain.NetParams
		want   string
	}{
		{chain.MainNet, "powm1q"},
		{chain.TestNet, "tpowm1q"},
		{chain.RegTest, "rpowm1q"},
	}
	for _, c := range cases {
		addr, err := Encode(c.params.Bech32HRP, chain.ScriptVersionBasic, key)
		if err != nil {
			t.Fatalf("%s: %v", c.params.Name, err)
		}
		if !strings.HasPrefix(addr, c.want) {
			t.Fatalf("%s: the address has to start with %q, it starts with %q", c.params.Name, c.want, addr[:8])
		}
		// The address must be readable back and must not be valid on another network.
		if _, _, err := Decode(c.params.Bech32HRP, addr); err != nil {
			t.Fatalf("%s: our own address cannot be read back: %v", c.params.Name, err)
		}
		for _, other := range cases {
			if other.params.Bech32HRP == c.params.Bech32HRP {
				continue
			}
			if _, _, err := Decode(other.params.Bech32HRP, addr); err == nil {
				t.Fatalf("an address of the %s network passed as %s", c.params.Name, other.params.Name)
			}
		}
		t.Logf("%-8s %s", c.params.Name, addr)
	}
}
