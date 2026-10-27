package main

import (
	"fmt"
	"testing"
	"time"

	"powmachina/internal/chain"
	"powmachina/internal/p2p"
)

// rawPeer opens a connection to the node and finishes the handshake.
func rawPeer(t *testing.T, addr p2p.Address, nonce uint64) *p2p.Conn {
	t.Helper()
	conn, err := p2p.Dial(fmt.Sprintf("%s:%d", addr.IP, addr.Port), chain.RegTest.Magic)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	ver := &p2p.VersionMsg{Protocol: p2p.ProtocolVersion, Timestamp: time.Now().Unix(),
		Nonce: nonce, UserAgent: "/test/"}
	if _, err := conn.Handshake(ver, nonce, true); err != nil {
		t.Fatal(err)
	}
	return conn
}

// A finality attestation may be forwarded **once**. Forwarded every time, gossip between three
// or more nodes loops forever and floods the peers — it ends in a ban for flooding and the
// connections falling apart.
//
// The test measures it by ordering on one connection: after the forwarded attestation the
// second peer sends a ping. If the node forwarded a duplicate, it would arrive before the pong.
func TestAttestationIsForwardedOnlyOnce(t *testing.T) {
	node, addr, cleanup := listeningNode(t)
	defer cleanup()
	node.noAttest = true
	if err := node.MineBlocks(finalityBlocks); err != nil {
		t.Fatal(err)
	}
	_, height, _ := node.chain.Tip()
	target := height - chain.FinalityDepth
	id, ok := node.chain.BlockIDAt(target)
	if !ok {
		t.Fatalf("we do not have the block at height %d", target)
	}

	sender := rawPeer(t, addr, 0xA11CE)
	watcher := rawPeer(t, addr, 0xB0B)
	// The handshake is finished on OUR side, but the node files the connection into `conns`
	// only after it, in its own goroutine. If the attestation were sent earlier, it would have
	// nobody to forward it to and the test would wait until ReadTimeout (90 s) for a message
	// that never came into existence. Under load this happens reliably.
	waitFor(t, 5*time.Second, func() bool {
		node.mu.Lock()
		defer node.mu.Unlock()
		return len(node.conns) == 2
	}, "the node did not register both connections")

	att := chain.Sign(node.signer.Priv(), node.chain.GenesisID(), target, id)
	payload, err := p2p.EncodeAttestation(att)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ { // the same attestation three times
		if err := sender.Send(&p2p.Message{Command: p2p.CmdAttestation, Payload: payload}); err != nil {
			t.Fatal(err)
		}
	}

	msg, err := watcher.Recv()
	if err != nil {
		t.Fatalf("the attestation was not forwarded: %v", err)
	}
	if msg.Command != p2p.CmdAttestation {
		t.Fatalf("expected an attestation, got %q", msg.Command)
	}

	// The ping goes after any further attestations — if a pong arrives, nothing else was sent.
	nonce := []byte{1, 1, 2, 2, 3, 3, 4, 4}
	if err := watcher.Send(&p2p.Message{Command: p2p.CmdPing, Payload: nonce}); err != nil {
		t.Fatal(err)
	}
	next, err := watcher.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if next.Command == p2p.CmdAttestation {
		t.Fatal("the node forwarded a duplicate attestation — gossip would loop")
	}
	if next.Command != p2p.CmdPong {
		t.Fatalf("expected a pong, got %q", next.Command)
	}
}

// Random bytes where an attestation should be must not knock the connection over or flood the network.
func TestBadAttestationIsNotForwarded(t *testing.T) {
	node, addr, cleanup := listeningNode(t)
	defer cleanup()

	sender := rawPeer(t, addr, 0xC0FFEE)
	watcher := rawPeer(t, addr, 0xDECAF)
	// The wait matters even more here than in the test above: this test claims that **nothing**
	// was forwarded. If the node did not have the watcher in `conns` yet, it would pass even if
	// duplicates were being forwarded — and that is worse than a failing test.
	waitFor(t, 5*time.Second, func() bool {
		node.mu.Lock()
		defer node.mu.Unlock()
		return len(node.conns) == 2
	}, "the node did not register both connections")

	var junk [p2p.AttestationBytes]byte
	junk[0] = 7
	if err := sender.Send(&p2p.Message{Command: p2p.CmdAttestation, Payload: junk[:]}); err != nil {
		t.Fatal(err)
	}
	nonce := []byte{9, 9, 9, 9, 9, 9, 9, 9}
	if err := watcher.Send(&p2p.Message{Command: p2p.CmdPing, Payload: nonce}); err != nil {
		t.Fatal(err)
	}
	msg, err := watcher.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if msg.Command != p2p.CmdPong {
		t.Fatalf("an attestation with a bad signature was forwarded (%q)", msg.Command)
	}
}
