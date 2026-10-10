package p2p

import (
	"testing"
	"time"

	"powmachina/internal/chain"
	"powmachina/internal/mh"
)

// regtestGenesis is the common root for both nodes in the test.
func regtestGenesis() *chain.Header {
	return &chain.Header{
		Version: 1, Time: 1_700_000_000, Bits: chain.RegtestTargetBits,
		Height: 0, Nonce: 0, Lane: mh.LaneC,
	}
}

// testHasher builds a hasher that knows both lanes. Lane C is RandomX, so it needs an epoch context
// — in light mode (~256 MiB), which is enough for a few hashes in a test.
func testHasher(t *testing.T) (*mh.Hasher, func()) {
	t.Helper()
	rx, err := mh.NewRandomX(0, false)
	if err != nil {
		t.Skipf("RandomX cannot be initialized: %v", err)
	}
	h, err := mh.NewHasherWithRandomX(rx)
	if err != nil {
		rx.Close()
		t.Fatal(err)
	}
	return h, func() { h.Close(); rx.Close() }
}

// mineNext finds a nonce for the next block of a given lane and adds it to the chain.
func mineNext(t *testing.T, hc *chain.HeaderChain, lane mh.Lane,
	hasher *mh.Hasher, v *mh.Verifier) *chain.Header {
	t.Helper()

	_, _, tipID := hc.Tip()
	tipHeader, height, _ := hc.Tip()
	bits, err := hc.NextBitsFor(lane)
	if err != nil {
		t.Fatal(err)
	}
	h := &chain.Header{
		Version: 1, PrevBlock: tipID, Time: tipHeader.Time + 130,
		Bits: bits, Height: uint32(height + 1), Lane: lane,
	}
	if mtp := hc.MedianTimePast(); h.Time <= mtp {
		h.Time = mtp + 1
	}
	for nonce := uint64(0); nonce < 200_000; nonce++ {
		h.Nonce = nonce
		ok, err := h.CheckPoW(hasher, v)
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			if _, err := hc.AddHeader(h, chain.PoWPair{Hasher: hasher, Verifier: v}); err != nil {
				t.Fatalf("our own mined block failed validation: %v", err)
			}
			return h
		}
	}
	t.Fatalf("no nonce found for lane %s", lane)
	return nil
}

// This is the first test where the network matters: two nodes over TCP shake hands and one pulls the
// headers from the other — including full validation (difficulty, time, lane rotation, PoW).
func TestTwoNodesHandshakeAndSyncHeaders(t *testing.T) {
	params := chain.RegTest
	verifier := mh.NewVerifier(0, mh.TestProfile)
	defer verifier.Close()
	hasher, closeHasher := testHasher(t)
	defer closeHasher()

	// Node A has a chain of six blocks; the lanes alternate because of the rotation rule.
	chainA := chain.NewHeaderChain(params, regtestGenesis(), nil)
	for i, lane := range []mh.Lane{mh.LaneG, mh.LaneG, mh.LaneC, mh.LaneG, mh.LaneG, mh.LaneC} {
		mineNext(t, chainA, lane, hasher, verifier)
		_, height, _ := chainA.Tip()
		if height != uint64(i+1) {
			t.Fatalf("after %d blocks the height is %d", i+1, height)
		}
	}
	_, heightA, tipA := chainA.Tip()

	// Node B knows only the genesis.
	chainB := chain.NewHeaderChain(params, regtestGenesis(), nil)

	ln, err := Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	// The server = node A: it answers getheaders with the headers of its branch.
	serverErr := make(chan error, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			serverErr <- err
			return
		}
		conn := NewConn(c, params.Magic)
		defer conn.Close()

		ver := &VersionMsg{Protocol: ProtocolVersion, Services: ServiceFullNode | ServiceFullDag,
			Timestamp: time.Now().Unix(), Nonce: 0xAAAA, UserAgent: "/A/", StartHeight: uint32(heightA)}
		if _, err := conn.Handshake(ver, 0xAAAA, false); err != nil {
			serverErr <- err
			return
		}
		msg, err := conn.Recv()
		if err != nil {
			serverErr <- err
			return
		}
		if msg.Command != CmdGetHeaders {
			serverErr <- ErrBadPayload
			return
		}
		locator, err := DecodeGetHeaders(msg.Payload)
		if err != nil {
			serverErr <- err
			return
		}
		payload, err := EncodeHeaders(chainA.HeadersFrom(locator, MaxHeadersPerMessage))
		if err != nil {
			serverErr <- err
			return
		}
		serverErr <- conn.Send(&Message{Command: CmdHeaders, Payload: payload})
	}()

	// The client = node B.
	conn, err := Dial(ln.Addr().String(), params.Magic)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	ver := &VersionMsg{Protocol: ProtocolVersion, Services: ServiceFullNode,
		Timestamp: time.Now().Unix(), Nonce: 0xBBBB, UserAgent: "/B/"}
	remote, err := conn.Handshake(ver, 0xBBBB, true)
	if err != nil {
		t.Fatalf("the handshake failed: %v", err)
	}
	if remote.UserAgent != "/A/" || remote.StartHeight != uint32(heightA) {
		t.Fatalf("the handshake returned unexpected data: %+v", remote)
	}

	locator, err := EncodeGetHeaders(chainB.Locator())
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Send(&Message{Command: CmdGetHeaders, Payload: locator}); err != nil {
		t.Fatal(err)
	}
	msg, err := conn.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if msg.Command != CmdHeaders {
		t.Fatalf("expected headers, got %q", msg.Command)
	}
	headers, err := DecodeHeaders(msg.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if len(headers) != int(heightA) {
		t.Fatalf("expected %d headers, got %d", heightA, len(headers))
	}

	// Node B verifies and files them — this is full consensus validation, not just taking them over.
	for _, h := range headers {
		if _, err := chainB.AddHeader(h, chain.PoWPair{Hasher: hasher, Verifier: verifier}); err != nil {
			t.Fatalf("the header at height %d did not pass: %v", h.Height, err)
		}
	}
	_, heightB, tipB := chainB.Tip()
	if heightB != heightA || tipB != tipA {
		t.Fatalf("the nodes did not agree: A=%d/%x B=%d/%x", heightA, tipA[:4], heightB, tipB[:4])
	}
	if err := <-serverErr; err != nil {
		t.Fatalf("side A reports an error: %v", err)
	}
	t.Logf("the two nodes agreed on height %d, tip %x", heightB, tipB[:8])
}

// A node must not accept a header with a faked difficulty or one outside the lane rotation rule.
func TestHeaderValidationRejectsCheating(t *testing.T) {
	params := chain.RegTest
	verifier := mh.NewVerifier(0, mh.TestProfile)
	defer verifier.Close()
	hasher, closeHasher := testHasher(t)
	defer closeHasher()

	hc := chain.NewHeaderChain(params, regtestGenesis(), nil)
	mineNext(t, hc, mh.LaneG, hasher, verifier)
	mineNext(t, hc, mh.LaneG, hasher, verifier)

	tip, height, tipID := hc.Tip()

	// A third block of the same lane in a row is invalid.
	third := &chain.Header{Version: 1, PrevBlock: tipID, Time: tip.Time + 130,
		Bits: chain.RegtestTargetBits, Height: uint32(height + 1), Lane: mh.LaneG}
	found := false
	for nonce := uint64(0); nonce < 200_000; nonce++ {
		third.Nonce = nonce
		ok, _ := third.CheckPoW(hasher, verifier)
		if ok {
			found = true
			break
		}
	}
	if !found {
		t.Skip("no nonce found")
	}
	if _, err := hc.AddHeader(third, chain.PoWPair{Hasher: hasher, Verifier: verifier}); err != chain.ErrLaneNotAllowed {
		t.Fatalf("a third identical lane should give ErrLaneNotAllowed, got %v", err)
	}

	// A faked difficulty (an easier target than the retarget demands).
	easier := &chain.Header{Version: 1, PrevBlock: tipID, Time: tip.Time + 130,
		Bits: 0x2100ffff, Height: uint32(height + 1), Lane: mh.LaneC}
	if _, err := hc.AddHeader(easier, chain.PoWPair{Hasher: hasher, Verifier: verifier}); err == nil {
		t.Fatal("a header with a faked difficulty passed")
	}

	// A time deep in the past.
	old := &chain.Header{Version: 1, PrevBlock: tipID, Time: 1,
		Bits: chain.RegtestTargetBits, Height: uint32(height + 1), Lane: mh.LaneC}
	if _, err := hc.AddHeader(old, chain.PoWPair{Hasher: hasher, Verifier: verifier}); err != chain.ErrTimeTooOld {
		t.Fatalf("an old time should give ErrTimeTooOld, got %v", err)
	}

	// An unknown predecessor.
	orphan := &chain.Header{Version: 1, PrevBlock: [32]byte{9, 9, 9}, Time: tip.Time + 130,
		Bits: chain.RegtestTargetBits, Height: uint32(height + 1), Lane: mh.LaneC}
	if _, err := hc.AddHeader(orphan, chain.PoWPair{Hasher: hasher, Verifier: verifier}); err != chain.ErrUnknownParent {
		t.Fatalf("an unknown predecessor should give ErrUnknownParent, got %v", err)
	}
}
