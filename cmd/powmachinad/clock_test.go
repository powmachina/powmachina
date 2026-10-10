package main

import (
	"net"
	"strconv"
	"testing"
	"time"

	"powmachina/internal/chain"
	"powmachina/internal/p2p"
)

// A node has to remember from the handshake how far its clock differs from its peers'. Without
// it a machine with a wrong clock cuts itself off silently: the network rejects its blocks, it
// rejects theirs — and it has no way of finding out why.
func TestNodeLearnsClockSkewFromPeers(t *testing.T) {
	node, addr, cleanup := listeningNode(t)
	defer cleanup()

	if _, n := node.ClockSkew(); n != 0 {
		t.Fatalf("a fresh node should have zero samples, it has %d", n)
	}

	// Five peers with a time two minutes ahead.
	for i := 0; i < 5; i++ {
		conn, err := p2p.Dial(net.JoinHostPort(addr.IP.String(), strconv.Itoa(int(addr.Port))), chain.RegTest.Magic)
		if err != nil {
			t.Fatal(err)
		}
		ver := &p2p.VersionMsg{Protocol: p2p.ProtocolVersion,
			Timestamp: time.Now().Add(2 * time.Minute).Unix(),
			Nonce:     uint64(0x1000 + i), UserAgent: "/shifted/"}
		if _, err := conn.Handshake(ver, uint64(0x1000+i), true); err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		skew, n := node.ClockSkew()
		if n >= 5 {
			if skew < 90*time.Second || skew > 150*time.Second {
				t.Fatalf("expected an offset of around 2 min, got %v", skew)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the node did not remember the peers' time (%d samples)", n)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
