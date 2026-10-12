package wallet

import (
	"testing"
	"time"

	"powmachina/internal/chain"
	"powmachina/internal/wallet/wallettest"
)

func noGrace(t *testing.T) {
	t.Helper()
	old := PendingGrace
	PendingGrace = -time.Second
	t.Cleanup(func() { PendingGrace = old })
}

// minedMeanwhile is a node on which a block arrives between the wallet's scan and its look into the
// mempool. The block takes everything waiting, so the mempool is empty and a rebroadcast is refused:
// the inputs are spent — by the very transaction being offered.
type minedMeanwhile struct{ *wallettest.Node }

func (m minedMeanwhile) Mempool() ([][32]byte, error) {
	m.Node.Mine(nil, nil)
	m.Node.Reject = true
	return m.Node.Mempool()
}

// A payment that was mined a moment ago must not be mistaken for one the node refuses. Forgetting it
// makes gettransaction deny a confirmed payout until the next sync, and the record that comes back from
// the block has lost the raw transaction — so a reorg of that block would delete it for good.
func TestAPaymentMinedDuringTheCheckIsKept(t *testing.T) {
	w, n := fundedWallet(t)
	to := pubOf(t, w, must(w.NewAddress("")))
	tx, _, err := w.Send(n, []Payment{{To: to, Amount: 10 * chain.POWM}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	noGrace(t)
	mustSync(t, w, minedMeanwhile{n})
	if _, ok := w.Transaction(tx.TxID()); !ok {
		t.Fatal("the wallet forgot a payment that had just been mined")
	}
	if b := w.Balance(1); b.Pending != 40*chain.POWM {
		t.Fatalf("the coins of a mined payment were freed: %+v", b)
	}
	mustSync(t, w, n)
	v, ok := w.Transaction(tx.TxID())
	if !ok || v.Confirmations != 1 {
		t.Fatalf("after reading the block: %+v", v)
	}
	if v.Raw == nil || len(v.Inputs) == 0 {
		t.Fatal("the confirmed payment lost its raw transaction — a reorg could not bring it back")
	}
}

type missingParent struct{}

func (missingParent) Error() string  { return "the transaction spends an output the node does not know" }
func (missingParent) Rejected() bool { return true }

// restarted is a node that came back without its mempool and, like the real one, refuses a transaction
// whose unconfirmed parent it has not been given yet.
type restarted struct {
	*wallettest.Node
	unconfirmed map[[32]byte]bool
}

func (r restarted) Broadcast(tx *chain.Tx) error {
	for _, in := range tx.Inputs {
		if _, have := r.Node.Pool[in.PrevTxID]; r.unconfirmed[in.PrevTxID] && !have {
			return missingParent{}
		}
	}
	return r.Node.Broadcast(tx)
}

// After a node restart the whole chain of unconfirmed payments has to go back in, parents first.
func TestAChainOfPaymentsIsSentAgainParentsFirst(t *testing.T) {
	w, n := fundedWallet(t)
	to := pubOf(t, w, must(w.NewAddress("")))
	sent := make(map[[32]byte]bool)
	for range 10 { // two confirmed coins, so from the third on every payment spends unconfirmed change
		tx, _, err := w.Send(n, []Payment{{To: to, Amount: chain.POWM}}, 0)
		if err != nil {
			t.Fatal(err)
		}
		sent[tx.TxID()] = true
	}
	noGrace(t)
	n.Pool = make(map[[32]byte]*chain.Tx)
	mustSync(t, w, restarted{Node: n, unconfirmed: sent})
	for id := range sent {
		if _, ok := w.Transaction(id); !ok {
			t.Fatalf("the wallet dropped payment %x because it was offered before its parent", id[:4])
		}
		if _, ok := n.Pool[id]; !ok {
			t.Fatalf("payment %x did not get back to the node", id[:4])
		}
	}
}

// A node with no room right now has said nothing about the transaction itself.
func TestAPaymentSurvivesAFullMempool(t *testing.T) {
	w, n := fundedWallet(t)
	to := pubOf(t, w, must(w.NewAddress("")))
	parent, _, err := w.Send(n, []Payment{{To: to, Amount: 39 * chain.POWM}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := w.Send(n, []Payment{{To: to, Amount: 39 * chain.POWM}}, 0); err != nil {
		t.Fatal(err)
	}
	child, _, err := w.Send(n, []Payment{{To: to, Amount: 10 * chain.POWM}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	noGrace(t)
	n.Pool = make(map[[32]byte]*chain.Tx)
	n.Busy = true
	mustSync(t, w, n)
	for _, tx := range []*chain.Tx{parent, child} {
		if _, ok := w.Transaction(tx.TxID()); !ok {
			t.Fatal("the wallet dropped a payment the node only had no room for")
		}
	}
	n.Busy = false
	mustSync(t, w, n)
	if len(n.Pool) != 3 {
		t.Fatalf("%d of 3 payments got back to the node once it had room", len(n.Pool))
	}
}
