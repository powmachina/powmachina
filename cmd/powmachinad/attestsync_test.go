package main

import (
	"encoding/binary"
	"fmt"
	"math"
	"net"
	"testing"
	"time"

	"powmachina/internal/chain"
	"powmachina/internal/mh"
	"powmachina/internal/p2p"
)

// A node that restarted has to **ask** for the attestations it is missing. Duplicates are
// deliberately not relayed, so the peers will not send them again on their own — without the
// request its finality would lag until the quorum signed the next height (with a depth of 20
// blocks, up to 40 minutes). When every node restarts at once — a new binary rolled out
// everywhere — finality reads "—" and everybody waits for the next height.
func TestNodeRequestsMissingAttestations(t *testing.T) {
	requestsMissingAttestations(t, finalityBlocks)
}

// The same on a chain longer than one request covers. The source keeps attestations from its
// finalized height up and nothing below, so a node that asked for the oldest heights it lacks got
// empty answers for ever.
func TestNodeRequestsTheNewestAttestationsOnALongChain(t *testing.T) {
	requestsMissingAttestations(t, finalityBlocks+p2p.MaxAttestHeights+7)
}

func requestsMissingAttestations(t *testing.T, blocks int) {
	// A node that has the attestations.
	source, srcAddr, cleanup := listeningNode(t)
	defer cleanup()
	if err := source.MineBlocks(blocks); err != nil {
		t.Fatal(err)
	}
	_, height, _ := source.chain.Tip()
	target := height - chain.FinalityDepth
	id, ok := source.chain.BlockIDAt(target)
	if !ok {
		t.Fatalf("we do not have the block at height %d", target)
	}
	// The source mined every block, so it is the quorum and signed each one as it went.
	if got, _, ok := source.finality.Finalized(); !ok || got != target {
		t.Fatalf("the source has no finality: %d (%v)", got, ok)
	}

	// A fresh node with the same chain but without the attestations — as after a restart.
	fresh := NewNode(chain.RegTest, chain.RegtestGenesis(), mh.TestProfile)
	defer fresh.Close()
	fresh.noAttest = true
	feedBranch(t, fresh, source, 1, height)
	if _, _, ok := fresh.finality.Finalized(); ok {
		t.Fatal("the fresh node must not have finality")
	}

	fresh.addrs.Add(srcAddr, net.ParseIP("88.198.113.1"))
	if err := fresh.FillOutboundOnce(p2p.PeerOutboundFull); err != nil {
		t.Fatal(err)
	}

	// The request is sent by the driver itself — the test goes through the same path as a
	// running node, so if the hook in the driver were lost, the test would fail.
	stop := make(chan struct{})
	defer close(stop)
	fresh.startLoop(stop, 50*time.Millisecond, func() { fresh.SyncBodiesOnce(time.Now()) })

	deadline := time.Now().Add(10 * time.Second)
	for {
		if got, gotID, ok := fresh.finality.Finalized(); ok {
			if got != target || gotID != id {
				t.Fatalf("finality is %d (%x), expected %d (%x)", got, gotID[:6], target, id[:6])
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the node did not request the attestations (finality stayed unconfirmed)")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// A request for attestations must not be a way to force a big answer or to knock the node over.
func TestGetAttestIsBounded(t *testing.T) {
	node, addr, cleanup := listeningNode(t)
	defer cleanup()
	_ = node
	conn := rawPeer(t, addr, 0xFEEDFACE)

	// A valid request against an empty state → an empty answer.
	payload, err := p2p.EncodeGetAttest(1, p2p.MaxAttestHeights)
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Send(&p2p.Message{Command: p2p.CmdGetAttest, Payload: payload}); err != nil {
		t.Fatal(err)
	}
	msg, err := conn.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if msg.Command != p2p.CmdAttestations {
		t.Fatalf("expected an attests answer, got %q", msg.Command)
	}
	list, err := p2p.DecodeAttestations(msg.Payload)
	if err != nil || len(list) != 0 {
		t.Fatalf("expected an empty list, got %d (%v)", len(list), err)
	}

	// A range that starts at the last possible height is answered at once. It used to send the node
	// walking 2^64 heights with the finality lock held.
	for _, req := range [][2]uint64{{math.MaxUint64, 1}, {math.MaxUint64, p2p.MaxAttestHeights}} {
		raw := make([]byte, 10)
		binary.LittleEndian.PutUint64(raw, req[0])
		binary.LittleEndian.PutUint16(raw[8:], uint16(req[1]))
		if err := conn.Send(&p2p.Message{Command: p2p.CmdGetAttest, Payload: raw}); err != nil {
			t.Fatal(err)
		}
		answered := make(chan error, 1)
		go func() {
			msg, err := conn.Recv()
			if err == nil && msg.Command != p2p.CmdAttestations {
				err = fmt.Errorf("got %q", msg.Command)
			}
			answered <- err
		}()
		select {
		case err := <-answered:
			if err != nil {
				t.Fatalf("getattest from %d, %d heights: %v", req[0], req[1], err)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("getattest from %d, %d heights was never answered", req[0], req[1])
		}
	}

	// A range above the cap has to be refused and the connection hung up — not obeyed.
	bad := make([]byte, 10)
	bad[8], bad[9] = 0xff, 0xff
	if err := conn.Send(&p2p.Message{Command: p2p.CmdGetAttest, Payload: bad}); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Recv(); err == nil {
		t.Fatal("the node answered a request above the cap instead of disconnecting")
	}
}
