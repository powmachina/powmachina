package miner

import (
	"errors"
	"fmt"
	"io"
	"log"
	"testing"
	"time"

	"powmachina/internal/chain"
	"powmachina/internal/nodeclient"
	"powmachina/internal/work"
)

// shakyNode is fakeNode behind a connection that fails: `drop` submits get no answer and never
// arrive, `lose` submits arrive and their answer is lost, `refuse` makes the node itself say no.
type shakyNode struct {
	fakeNode
	drop, lose int
	refuse     bool
	calls      int
}

var errNoAnswer = errors.New("node 127.0.0.1:27697 is not reachable: i/o timeout")

func (n *shakyNode) SubmitBlock(b *chain.Block) error {
	n.calls++
	switch {
	case n.drop > 0:
		n.drop--
		return errNoAnswer
	case n.refuse:
		return fmt.Errorf("%w: the block does not extend the tip", nodeclient.ErrRPC)
	}
	if err := n.fakeNode.SubmitBlock(b); err != nil {
		// The node answers, and its answer is a refusal.
		return fmt.Errorf("%w: %v", nodeclient.ErrRPC, err)
	}
	if n.lose > 0 {
		n.lose--
		return errNoAnswer
	}
	return nil
}

func soloFeed(t *testing.T, n *shakyNode) (*NodeFeed, *work.Job) {
	t.Helper()
	n.tip = chain.RegtestGenesis().ID()
	f := &NodeFeed{Src: n, Payout: make([]byte, 32), Params: chain.RegTest,
		Retry: time.Millisecond, Log: log.New(io.Discard, "", 0)}
	tpl, _ := n.Template("C")
	j, err := work.Build(tpl, f.Payout, chain.RegTest)
	if err != nil {
		t.Fatal(err)
	}
	return f, j
}

// A block found solo is the whole reward. A submit that got no answer — a timeout, a node that was
// restarting — is sent again and not written off.
func TestBlockIsSentAgainWhenTheNodeDoesNotAnswer(t *testing.T) {
	n := &shakyNode{drop: 2}
	f, j := soloFeed(t, n)

	ok, block, err := f.Submit(j, 1)
	if err != nil || !ok || !block {
		t.Fatalf("accepted %v, block %v, err %v", ok, block, err)
	}
	if len(n.got) != 1 || n.got[0].Header.Nonce != 1 {
		t.Fatalf("the node has %d blocks", len(n.got))
	}
	if n.calls != 3 {
		t.Fatalf("the block was sent %d times, two sends were to fail", n.calls)
	}
}

// When the node stays silent for all the attempts, the job is not closed: the next nonce found for
// it is a block too, and it has to go out. It used to be counted as accepted without being sent.
func TestJobStaysOpenWhenItsBlockWasNotDelivered(t *testing.T) {
	n := &shakyNode{drop: 1 + submitRetries}
	f, j := soloFeed(t, n)

	if ok, block, err := f.Submit(j, 1); err == nil || ok || block {
		t.Fatalf("a block that was never delivered: accepted %v, block %v, err %v", ok, block, err)
	}
	if len(n.got) != 0 {
		t.Fatal("the node has a block it was never sent")
	}
	// The node is back. The second nonce of the same job is sent and taken.
	ok, block, err := f.Submit(j, 2)
	if err != nil || !ok || !block {
		t.Fatalf("the next nonce of the job: accepted %v, block %v, err %v", ok, block, err)
	}
	if len(n.got) != 1 || n.got[0].Header.Nonce != 2 {
		t.Fatalf("the node has %d blocks", len(n.got))
	}
	// Now the job is settled and a third nonce is not sent.
	before := n.calls
	if ok, block, _ := f.Submit(j, 3); !ok || block || n.calls != before {
		t.Fatalf("a nonce for a job the node already took a block of: accepted %v, block %v, sent %v", ok, block, n.calls != before)
	}
}

// A submit whose answer was lost did arrive. The second copy is refused as one the node has, and
// the block is still ours: the node's tip says so.
func TestBlockWhoseAnswerWasLostIsStillCounted(t *testing.T) {
	n := &shakyNode{lose: 1}
	f, j := soloFeed(t, n)

	ok, block, err := f.Submit(j, 1)
	if err != nil || !ok || !block {
		t.Fatalf("accepted %v, block %v, err %v", ok, block, err)
	}
	if len(n.got) != 1 {
		t.Fatalf("the node has %d blocks", len(n.got))
	}
}

// A refusal is an answer. The block is not sent again and neither are further nonces of that job —
// and they are not counted as accepted.
func TestBlockTheNodeRefusedIsNotSentAgain(t *testing.T) {
	n := &shakyNode{refuse: true}
	f, j := soloFeed(t, n)

	if ok, block, err := f.Submit(j, 1); err != nil || ok || block {
		t.Fatalf("a refused block: accepted %v, block %v, err %v", ok, block, err)
	}
	if ok, _, err := f.Submit(j, 2); err != nil || ok {
		t.Fatalf("a nonce of a refused job: accepted %v, err %v", ok, err)
	}
	if n.calls != 1 {
		t.Fatalf("the node was asked %d times", n.calls)
	}
}
