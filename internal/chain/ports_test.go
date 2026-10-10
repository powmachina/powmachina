package chain

import "testing"

// The ports follow one scheme: 7696 ("POWM" on a keypad), testnet +10000, regtest +20000, the
// services in order P2P, RPC, the block mirror, stratum. And all of them have to be below the range
// Linux assigns outgoing connections from — otherwise a random connection can take them after a
// restart and the node will not start.
func TestPortScheme(t *testing.T) {
	const ephemeralStart = 32768 // /proc/sys/net/ipv4/ip_local_port_range
	cases := []struct {
		p    NetParams
		base uint16
	}{
		{MainNet, 7696},
		{TestNet, 17696},
		{RegTest, 27696},
	}
	seen := map[uint16]string{}
	for _, c := range cases {
		if c.p.WalletRPCPort != c.base-1 {
			t.Errorf("%s: wallet-rpc has port %d, the scheme says %d", c.p.Name, c.p.WalletRPCPort, c.base-1)
		}
		if other, dup := seen[c.p.WalletRPCPort]; dup {
			t.Errorf("port %d is used by both %s and %s", c.p.WalletRPCPort, other, c.p.Name)
		}
		seen[c.p.WalletRPCPort] = c.p.Name
		got := []uint16{c.p.P2PPort, c.p.RPCPort, c.p.BlocksPort, c.p.StratumPort}
		for i, port := range got {
			if want := c.base + uint16(i); port != want {
				t.Errorf("%s: the port of service %d is %d, the scheme says %d", c.p.Name, i, port, want)
			}
			if port >= ephemeralStart {
				t.Errorf("%s: port %d is in Linux's outgoing connection range", c.p.Name, port)
			}
			if other, dup := seen[port]; dup {
				t.Errorf("port %d is used by both %s and %s", port, other, c.p.Name)
			}
			seen[port] = c.p.Name
		}
	}
}
