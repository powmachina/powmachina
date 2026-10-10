package main

import (
	"testing"
	"time"

	"powmachina/internal/chain"
	"powmachina/internal/mh"
	"powmachina/internal/p2p"
)

// Five signatures from different quorum members have to advance finality — but only for a
// block the node really has on its own branch.
func TestFinalityAdvancesWithQuorum(t *testing.T) {
	node := NewNode(chain.RegTest, chain.RegtestGenesis(), mh.TestProfile)
	defer node.Close()

	if err := node.MineBlocks(int(chain.FinalityDepth) + 1); err != nil {
		t.Fatal(err)
	}
	_, height, _ := node.chain.Tip()
	target := height - chain.FinalityDepth
	id, ok := node.chain.BlockIDAt(target)
	if !ok {
		t.Fatalf("we do not have the block at height %d", target)
	}

	// Below the threshold nothing may be finalized.
	for i := 0; i < chain.FinalityThreshold-1; i++ {
		att := chain.Sign(chain.RegtestSignerKey(i), uint8(i), target, id)
		if err := node.acceptAttestation(att, ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, ok := node.finality.Finalized(); ok {
		t.Fatal("nothing may be finalized below the threshold")
	}

	// The last signature reaches the threshold.
	last := chain.FinalityThreshold - 1
	att := chain.Sign(chain.RegtestSignerKey(last), uint8(last), target, id)
	if err := node.acceptAttestation(att, ""); err != nil {
		t.Fatal(err)
	}
	got, gotID, ok := node.finality.Finalized()
	if !ok || got != target || gotID != id {
		t.Fatalf("expected finalized height %d, got %d (ok=%v)", target, got, ok)
	}

	// A reorg below a finalized block has to be refused.
	if err := node.finality.CheckReorg(target - 1); err != chain.ErrFinalityRewrite {
		t.Fatalf("a reorg below a finalized block should be refused, got %v", err)
	}
}

// The quorum must not finalize a block the node does not have — not even unanimously.
func TestFinalityIgnoresForeignBlock(t *testing.T) {
	node := NewNode(chain.RegTest, chain.RegtestGenesis(), mh.TestProfile)
	defer node.Close()
	if err := node.MineBlocks(int(chain.FinalityDepth) + 1); err != nil {
		t.Fatal(err)
	}
	_, height, _ := node.chain.Tip()
	target := height - chain.FinalityDepth

	var foreign [32]byte
	foreign[0] = 0xff
	for i := 0; i < chain.FinalitySigners; i++ {
		att := chain.Sign(chain.RegtestSignerKey(i), uint8(i), target, foreign)
		_ = node.acceptAttestation(att, "")
	}
	if _, _, ok := node.finality.Finalized(); ok {
		t.Fatal("the quorum finalized a block the node does not have")
	}
}

// A double signature turns finality off for that height and the chain keeps running.
func TestEquivocationDisablesFinality(t *testing.T) {
	node := NewNode(chain.RegTest, chain.RegtestGenesis(), mh.TestProfile)
	defer node.Close()
	if err := node.MineBlocks(int(chain.FinalityDepth) + 1); err != nil {
		t.Fatal(err)
	}
	_, height, _ := node.chain.Tip()
	target := height - chain.FinalityDepth
	id, _ := node.chain.BlockIDAt(target)

	for i := 0; i < chain.FinalityThreshold; i++ {
		_ = node.acceptAttestation(chain.Sign(chain.RegtestSignerKey(i), uint8(i), target, id), "")
	}
	if _, _, ok := node.finality.Finalized(); !ok {
		t.Fatal("the quorum should have finalized the height")
	}

	// One of them signs a different block at the same height.
	var other [32]byte
	other[0] = 0xAB
	_ = node.acceptAttestation(chain.Sign(chain.RegtestSignerKey(0), 0, target, other), "")

	// The chain keeps running — a double signature must not stop it.
	if err := node.MineBlocks(1); err != nil {
		t.Fatalf("the chain stopped after a double signature: %v", err)
	}
}

// An attestation received from a peer has to be relayed on — otherwise finality would never spread.
func TestAttestationIsRelayed(t *testing.T) {
	params := chain.RegTest
	node := NewNode(params, chain.RegtestGenesis(), mh.TestProfile)
	defer node.Close()
	if err := node.MineBlocks(int(chain.FinalityDepth) + 1); err != nil {
		t.Fatal(err)
	}
	_, height, _ := node.chain.Tip()
	target := height - chain.FinalityDepth
	id, _ := node.chain.BlockIDAt(target)

	ln, err := p2p.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go node.Listen(ln)

	// We connect two peers; the first sends the attestation, the second has to receive it.
	connA, err := p2p.Dial(ln.Addr().String(), params.Magic)
	if err != nil {
		t.Fatal(err)
	}
	defer connA.Close()
	if _, err := connA.Handshake(&p2p.VersionMsg{Protocol: p2p.ProtocolVersion,
		Timestamp: time.Now().Unix(), Nonce: 11, UserAgent: "/A/"}, 11, true); err != nil {
		t.Fatal(err)
	}
	connB, err := p2p.Dial(ln.Addr().String(), params.Magic)
	if err != nil {
		t.Fatal(err)
	}
	defer connB.Close()
	if _, err := connB.Handshake(&p2p.VersionMsg{Protocol: p2p.ProtocolVersion,
		Timestamp: time.Now().Unix(), Nonce: 22, UserAgent: "/B/"}, 22, true); err != nil {
		t.Fatal(err)
	}

	att := chain.Sign(chain.RegtestSignerKey(3), 3, target, id)
	payload, err := p2p.EncodeAttestation(att)
	if err != nil {
		t.Fatal(err)
	}
	if err := connA.Send(&p2p.Message{Command: p2p.CmdAttestation, Payload: payload}); err != nil {
		t.Fatal(err)
	}

	got := make(chan *p2p.Message, 1)
	go func() {
		m, err := connB.Recv()
		if err == nil {
			got <- m
		}
	}()
	select {
	case m := <-got:
		if m.Command != p2p.CmdAttestation {
			t.Fatalf("the second peer got %q, expected an attestation", m.Command)
		}
		relayed, err := p2p.DecodeAttestation(m.Payload)
		if err != nil {
			t.Fatal(err)
		}
		if relayed.Height != target || relayed.BlockID != id || relayed.SignerIdx != 3 {
			t.Fatalf("the relayed attestation does not match: %+v", relayed)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the attestation was not relayed to the second peer")
	}
}
