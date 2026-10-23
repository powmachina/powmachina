package wallet

import (
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"powmachina/internal/chain"
	"powmachina/internal/wallet/wallettest"
)

// The node took the payment and its answer got lost. The wallet must come out of that knowing the
// transaction and holding its coins — otherwise the next payment (the caller's retry above all) is built
// from other coins and the recipient is paid twice.
func TestALostReplyDoesNotPayTwice(t *testing.T) {
	w, n := fundedWallet(t)
	other, _ := testWallet(t)
	to := pubOf(t, other, must(other.NewAddress("")))

	n.LoseReply = true
	tx, fee, err := w.Send(n, []Payment{{To: to, Amount: 5 * chain.POWM}}, 0)
	if !errors.Is(err, ErrBroadcastUnknown) || tx == nil {
		t.Fatalf("a lost reply has to come back as ErrBroadcastUnknown with the transaction: %v, %v", tx, err)
	}
	n.LoseReply = false
	if v, ok := w.Transaction(tx.TxID()); !ok || v.Confirmations != 0 || v.Raw == nil {
		t.Fatalf("the wallet does not know the payment it sent: %+v", v)
	}
	if b := w.Balance(1); b.Pending != 40*chain.POWM || b.Spendable != 40*chain.POWM {
		t.Fatalf("the coins of a payment with an unknown outcome are not reserved: %+v", b)
	}

	n.Mine(nil, nil)
	mustSync(t, w, n)
	mustSync(t, other, n)
	if got := other.Balance(1).Spendable; got != 5*chain.POWM {
		t.Fatalf("the recipient has %s for one payment of 5", FormatAmount(got))
	}
	if v, ok := w.Transaction(tx.TxID()); !ok || v.Confirmations != 1 {
		t.Fatalf("the payment is not confirmed: %+v", v)
	}
	if b := w.Balance(1); b.Spendable != 75*chain.POWM-fee || b.Pending != 0 {
		t.Fatalf("after confirmation: %+v", b)
	}
}

// The node never got the payment and never answered. The wallet keeps it for as long as the node stays
// silent and hands it over as soon as the node listens again.
func TestAPaymentTheNodeNeverHeardOfIsSentAgain(t *testing.T) {
	w, n := fundedWallet(t)
	to := pubOf(t, w, must(w.NewAddress("")))
	old := PendingGrace
	PendingGrace = -time.Second
	defer func() { PendingGrace = old }()

	n.Silent = true
	tx, _, err := w.Send(n, []Payment{{To: to, Amount: 5 * chain.POWM}}, 0)
	if !errors.Is(err, ErrBroadcastUnknown) || tx == nil {
		t.Fatalf("a node that does not answer: %v, %v", tx, err)
	}
	mustSync(t, w, n)
	if _, ok := w.Transaction(tx.TxID()); !ok {
		t.Fatal("the wallet dropped a payment because the node did not answer")
	}
	if b := w.Balance(1); b.Pending != 40*chain.POWM {
		t.Fatalf("the coins were freed while the outcome is unknown: %+v", b)
	}

	n.Silent = false
	mustSync(t, w, n)
	if _, ok := n.Pool[tx.TxID()]; !ok {
		t.Fatal("the payment was not sent again once the node answered")
	}
}

// The node said no. Nobody has the transaction, so nothing of it stays: no record, no reserved coin.
func TestARefusedPaymentLeavesNoTrace(t *testing.T) {
	w, n := fundedWallet(t)
	to := pubOf(t, w, must(w.NewAddress("")))
	n.Reject = true
	tx, _, err := w.Send(n, []Payment{{To: to, Amount: 5 * chain.POWM}}, 0)
	if err == nil || errors.Is(err, ErrBroadcastUnknown) || tx != nil {
		t.Fatalf("a refused payment: %v, %v", tx, err)
	}
	if b := w.Balance(1); b.Spendable != 80*chain.POWM || b.Pending != 0 || b.Incoming != 0 {
		t.Fatalf("a refused payment still holds coins: %+v", b)
	}
	if len(w.Transactions()) != 2 {
		t.Fatalf("%d transactions, expected the two rewards", len(w.Transactions()))
	}
	// The coins are usable right away.
	n.Reject = false
	if _, _, err := w.Send(n, []Payment{{To: to, Amount: 5 * chain.POWM}}, 0); err != nil {
		t.Fatal(err)
	}
}

// diskCheck is a node that looks into the wallet file at the moment the transaction reaches it.
type diskCheck struct {
	*wallettest.Node
	w      *Wallet
	onDisk bool
}

func (d *diskCheck) Broadcast(tx *chain.Tx) error {
	data, err := os.ReadFile(d.w.path)
	if err != nil {
		return err
	}
	f, err := parseFile(data)
	if err != nil {
		return err
	}
	plain, err := f.open(d.w.key)
	if err != nil {
		return err
	}
	var p payloadJSON
	if err := json.Unmarshal(plain, &p); err != nil {
		return err
	}
	id := tx.TxID()
	for _, t := range p.Txs {
		if a32(t.ID) == id && t.Raw != nil {
			d.onDisk = true
		}
	}
	return d.Node.Broadcast(tx)
}

// The record has to be in the file before the transaction leaves: a process killed between the two
// would otherwise restart without a payment that is already in the network.
func TestAPaymentIsOnDiskBeforeItIsBroadcast(t *testing.T) {
	w, n := fundedWallet(t)
	to := pubOf(t, w, must(w.NewAddress("")))
	d := &diskCheck{Node: n, w: w}
	if _, _, err := w.Send(d, []Payment{{To: to, Amount: 5 * chain.POWM}}, 0); err != nil {
		t.Fatal(err)
	}
	if !d.onDisk {
		t.Fatal("the transaction reached the node before the wallet file knew about it")
	}

	// The same holds for a transaction that was built first and confirmed by a person afterwards.
	d.onDisk = false
	tx, _, err := w.Build([]Payment{{To: to, Amount: 5 * chain.POWM}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Submit(d, tx); err != nil {
		t.Fatal(err)
	}
	if !d.onDisk {
		t.Fatal("a built transaction reached the node before the wallet file knew about it")
	}
}

// A sweep whose reply got lost is recorded too, and a second sweep does not touch its coins.
func TestASweepWithALostReplyKeepsItsCoins(t *testing.T) {
	w, n := fundedWallet(t)
	other, _ := testWallet(t)
	to := pubOf(t, other, must(other.NewAddress("")))
	n.LoseReply = true
	sent, err := w.Sweep(n, to, 1, 0, 0)
	if !errors.Is(err, ErrBroadcastUnknown) || len(sent) != 1 {
		t.Fatalf("a sweep with a lost reply: %d transactions, %v", len(sent), err)
	}
	n.LoseReply = false
	again, err := w.Sweep(n, to, 1, 0, 0)
	if err != nil || len(again) != 0 {
		t.Fatalf("the second sweep found coins the first one already spent: %d transactions, %v", len(again), err)
	}
	n.Mine(nil, nil)
	mustSync(t, other, n)
	if got := other.Balance(1).Spendable; got != sent[0].Outputs[0].Value {
		t.Fatalf("the destination has %s, the sweep sent %s", FormatAmount(got), FormatAmount(sent[0].Outputs[0].Value))
	}
}

// An output that carries the bytes of one of our keys under another script version is not ours: the
// block scan does not count it, so a recorded transaction must not either.
func TestAnOutputOfAnotherScriptVersionIsNotACoin(t *testing.T) {
	w, _ := fundedWallet(t)
	pk := pubOf(t, w, must(w.NewAddress("")))
	coin := w.Unspent(1, 9_999_999)[0]
	tx := &chain.Tx{Version: 1,
		Inputs: []chain.TxIn{{PrevTxID: coin.Outpoint.TxID, PrevIndex: coin.Outpoint.Index}},
		Outputs: []chain.TxOut{{Value: coin.Value - chain.POWM, ScriptVersion: chain.ScriptVersionData,
			Script: append([]byte{}, pk[:]...)}}}
	if err := w.Commit(tx); err != nil {
		t.Fatal(err)
	}
	if b := w.Balance(1); b.Incoming != 0 {
		t.Fatalf("the wallet counts %s it cannot spend", FormatAmount(b.Incoming))
	}
	if v, _ := w.Transaction(tx.TxID()); v.Outputs[0].Mine || v.Received != 0 {
		t.Fatalf("the output is recorded as ours: %+v", v.Outputs[0])
	}
}

// Somebody pays to an address from the change branch (getrawchangeaddress hands them out). That is a
// deposit, and the history has to show it as one.
func TestAPaymentToAChangeAddressIsNotHiddenAsChange(t *testing.T) {
	payer, n := fundedWallet(t)
	w, _ := testWallet(t)
	to := pubOf(t, w, must(w.ChangeAddress()))
	tx, _, err := payer.Send(n, []Payment{{To: to, Amount: 3 * chain.POWM}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	n.Mine(nil, nil)
	if res := mustSync(t, w, n); len(res.Received) != 1 {
		t.Fatalf("%d incoming payments reported, expected 1", len(res.Received))
	}
	v, ok := w.Transaction(tx.TxID())
	if !ok {
		t.Fatal("the wallet does not know the payment")
	}
	for _, o := range v.Outputs {
		if o.Mine && o.Change {
			t.Fatal("a payment from somebody else is marked as our own change")
		}
	}
	// Our own change is still change.
	mustSync(t, payer, n)
	v, _ = payer.Transaction(tx.TxID())
	changes := 0
	for _, o := range v.Outputs {
		if o.Mine && o.Change {
			changes++
		}
	}
	if changes != 1 {
		t.Fatalf("the payer sees %d change outputs in its own payment, expected 1", changes)
	}
}

// A small change weighs a lot (storage mass), so a smaller change needs a higher fee, which makes the
// change smaller still. Close to the dust limit no fee pays for such a change at all. Whatever the
// remainder, the payment has to pay the fee the node asks for — a pool paying out 18.69 POWM was
// turned away with 998 cogs per 1 000 units of weight.
func TestAPaymentPaysTheMinimumFeeWhateverTheRemainder(t *testing.T) {
	w, _ := fundedWallet(t)
	other, _ := testWallet(t)
	to := pubOf(t, other, must(other.NewAddress("")))
	for _, coins := range []int64{1, 2} {
		for rest := int64(0); rest < 400_000; rest += 97 {
			tx, fee, err := w.Build([]Payment{{To: to, Amount: coins*40*chain.POWM - rest}}, 0)
			if err != nil {
				if !errors.Is(err, ErrNoFunds) || coins == 1 {
					t.Fatalf("%d coins, remainder %d: %v", coins, rest, err)
				}
				continue
			}
			values := make([]int64, len(tx.Inputs))
			for i := range values {
				values[i] = 40 * chain.POWM
			}
			weight := chain.PolicyWeight(len(tx.Serialize()), values, tx.Outputs)
			if rate := fee * 1000 / int64(weight); rate < chain.MinRelayFeePerKB {
				t.Fatalf("%d coins, remainder %d: fee %d for weight %d = %d cogs per 1 000",
					coins, rest, fee, weight, rate)
			}
		}
	}
}
