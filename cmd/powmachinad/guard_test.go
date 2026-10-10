package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"powmachina/internal/chain"
	"powmachina/internal/mh"
	"powmachina/internal/p2p"
)

// A peer sends a header that passes every cheap check but has no valid PoW. An honest node
// cannot send one — the ban has to come at once, on the first attempt, and the IP has to show
// up in the file for the firewall.
func TestBadPoWBansAtOnceAndReachesFirewall(t *testing.T) {
	params := chain.RegTest
	genesis := chain.RegtestGenesis()
	node := NewNode(params, genesis, mh.TestProfile)
	defer node.Close()
	banFile := filepath.Join(t.TempDir(), "bans.txt")
	node.SetBanListPath(banFile)
	stop := make(chan struct{})
	defer close(stop)
	go node.writeBanListLoop(stop)

	ln, err := p2p.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go node.Listen(ln)

	// A header on top of the genesis with the target the node expects and a nonce that does NOT meet it.
	bits, err := node.chain.NextBitsFor(mh.LaneG)
	if err != nil {
		t.Fatal(err)
	}
	h := &chain.Header{Version: 1, PrevBlock: genesis.ID(), MerkleRoot: [32]byte{1},
		Time: genesis.Time + 120, Bits: bits, Height: 1, Lane: mh.LaneG}
	v := mh.NewVerifier(0, mh.TestProfile)
	hasher := mh.NewHasher()
	for {
		ok, err := h.CheckPoW(hasher, v)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			break
		}
		h.Nonce++
	}

	conn, err := p2p.Dial(ln.Addr().String(), params.Magic)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ver := &p2p.VersionMsg{Protocol: p2p.ProtocolVersion, Timestamp: time.Now().Unix(),
		Nonce: 0xBAD, UserAgent: "/test/"}
	if _, err := conn.Handshake(ver, 0xBAD, true); err != nil {
		t.Fatal(err)
	}
	payload, err := p2p.EncodeHeaders([]*chain.Header{h})
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Send(&p2p.Message{Command: p2p.CmdHeaders, Payload: payload}); err != nil {
		t.Fatal(err)
	}

	peerAddr := p2p.Address{IP: []byte{127, 0, 0, 1}, Port: 1}
	deadline := time.Now().Add(10 * time.Second)
	for !node.peers.IsBanned(peerAddr) {
		if time.Now().After(deadline) {
			t.Fatal("the peer with bad PoW was not banned")
		}
		time.Sleep(20 * time.Millisecond)
	}
	for {
		data, _ := os.ReadFile(banFile)
		if strings.Contains(string(data), "127.0.0.1\n") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the ban did not reach the file for the firewall, contents: %q", data)
		}
		time.Sleep(100 * time.Millisecond)
	}
	// Reconnecting from another source port no longer gets through the handshake.
	again, err := p2p.Dial(ln.Addr().String(), params.Magic)
	if err == nil {
		defer again.Close()
		if _, err := again.Handshake(ver, 0xBAD, true); err == nil {
			t.Fatal("the banned peer connected again")
		}
	}
}
