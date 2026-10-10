package wallet

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"powmachina/internal/bip39"
	"powmachina/internal/chain"
	"powmachina/internal/wallet/wallettest"
)

func init() {
	// Argon2id with 256 MiB would slow the tests down to minutes; the format is the same, only the cost changes.
	DefaultKDF = KDFParams{MemoryKiB: minMemoryKiB, Time: 1, Threads: 1}
}

var params = chain.RegTest

func testWallet(t *testing.T) (*Wallet, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mywallet")
	w, m, err := Create(path, "password", params, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { w.Close() })
	return w, m
}

func pubOf(t *testing.T, w *Wallet, addr string) [32]byte {
	t.Helper()
	pk, err := w.DecodeAddress(addr)
	if err != nil {
		t.Fatal(err)
	}
	return pk
}

func mustSync(t *testing.T, w *Wallet, n Node) *SyncResult {
	t.Helper()
	res, err := w.Sync(n)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// ------------------------------------------------------------------ the file

func TestFileRoundTripAndMnemonic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mywallet")
	w, m, err := Create(path, "password", params, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bip39.Entropy(m); err != nil || len(splitWords(m)) != 24 {
		t.Fatalf("mnemonic %q: %v", m, err)
	}
	a1, _ := w.NewAddress("first")
	if _, _, err := Create(path, "password", params, 0); !errors.Is(err, ErrExists) && !errors.Is(err, ErrLocked) {
		t.Fatalf("overwriting an existing wallet: %v", err)
	}
	if _, err := Open(path, "password", params); !errors.Is(err, ErrLocked) {
		t.Fatalf("a second open while running: %v", err)
	}
	w.Close()

	if _, err := Open(path, "wrong", params); !errors.Is(err, ErrWrongPassword) {
		t.Fatalf("a wrong password: %v", err)
	}
	if _, err := Open(path, "password", chain.MainNet); !errors.Is(err, ErrWrongNetwork) {
		t.Fatalf("another network: %v", err)
	}
	w2, err := Open(path, "password", params)
	if err != nil {
		t.Fatal(err)
	}
	if got := w2.Addresses(); len(got) != 1 || got[0].Address != a1 || got[0].Label != "first" {
		t.Fatalf("the addresses after opening: %+v", got)
	}
	w2.Close()

	// A restore from the words gives the same first address.
	w3, err := Restore(filepath.Join(t.TempDir(), "restore"), "other", m, params, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer w3.Close()
	if a, _ := w3.NewAddress(""); a != a1 {
		t.Fatalf("the restore from the words gave %s, the original is %s", a, a1)
	}
}

func splitWords(s string) []string {
	var out []string
	cur := ""
	for _, r := range s + " " {
		if r == ' ' {
			if cur != "" {
				out = append(out, cur)
			}
			cur = ""
			continue
		}
		cur += string(r)
	}
	return out
}

// A changed byte anywhere — in the header or in the content — has to fail like a wrong password.
func TestFileTamperingIsDetected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mywallet")
	w, _, err := Create(path, "password", params, 0)
	if err != nil {
		t.Fatal(err)
	}
	w.Close()
	orig, _ := os.ReadFile(path)
	for _, pos := range []int{8, 12, 20, 40, 60, headerBytes + 5, len(orig) - 1} {
		bad := append([]byte{}, orig...)
		bad[pos] ^= 0x01
		if err := os.WriteFile(path, bad, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(path, "password", params); err == nil {
			t.Fatalf("changing byte %d passed", pos)
		}
	}
	// Argon2 parameters out of bounds (a file that would eat all the memory when opened) — refuse before
	// the computation.
	bad := append([]byte{}, orig...)
	bad[15], bad[16], bad[17], bad[18] = 0xff, 0xff, 0xff, 0x7f
	os.WriteFile(path, bad, 0o600)
	if _, err := Open(path, "password", params); !errors.Is(err, ErrBadParams) {
		t.Fatalf("an enormous Argon2 memory: %v", err)
	}
}

func TestChangePassword(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mywallet")
	w, _, _ := Create(path, "old", params, 0)
	if err := w.ChangePassword("wrong", "new"); !errors.Is(err, ErrWrongPassword) {
		t.Fatalf("the change with a wrong password: %v", err)
	}
	if err := w.ChangePassword("old", "new"); err != nil {
		t.Fatal(err)
	}
	w.Close()
	if _, err := Open(path, "old", params); err == nil {
		t.Fatal("the old password still works")
	}
	w2, err := Open(path, "new", params)
	if err != nil {
		t.Fatal(err)
	}
	w2.Close()
}

// ------------------------------------------------------------------ sync and payments

func TestSyncFindsCoinbaseAndRespectsMaturity(t *testing.T) {
	w, _ := testWallet(t)
	n := wallettest.NewNode()
	pk := pubOf(t, w, must(w.NewAddress("")))
	n.Mine(&pk, nil)
	res := mustSync(t, w, n)
	if len(res.Received) != 1 {
		t.Fatalf("%d transactions received, expected 1", len(res.Received))
	}
	if b := w.Balance(1); b.Immature != 40*chain.POWM || b.Spendable != 0 {
		t.Fatalf("before maturity: %+v", b)
	}
	for range chain.CoinbaseMaturity {
		n.Mine(nil, nil)
	}
	mustSync(t, w, n)
	if b := w.Balance(1); b.Spendable != 40*chain.POWM {
		t.Fatalf("after maturity: %+v", b)
	}
}

func must(s string, err error) string {
	if err != nil {
		panic(err)
	}
	return s
}

// fundedWallet: a wallet with two matured rewards of 40 POWM each.
func fundedWallet(t *testing.T) (*Wallet, *wallettest.Node) {
	w, _ := testWallet(t)
	n := wallettest.NewNode()
	pk := pubOf(t, w, must(w.NewAddress("")))
	n.Mine(&pk, nil)
	n.Mine(&pk, nil)
	for range chain.CoinbaseMaturity {
		n.Mine(nil, nil)
	}
	mustSync(t, w, n)
	return w, n
}

func TestSendReservesThenConfirms(t *testing.T) {
	w, n := fundedWallet(t)
	other, _ := testWallet(t)
	to := pubOf(t, other, must(other.NewAddress("")))

	tx, fee, err := w.Send(n, []Payment{{To: to, Amount: 10 * chain.POWM}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if fee <= 0 {
		t.Fatalf("fee %d", fee)
	}
	b := w.Balance(1)
	if b.Pending != 40*chain.POWM || b.Spendable != 40*chain.POWM || b.Incoming != 30*chain.POWM-fee {
		t.Fatalf("after sending: %+v", b)
	}
	// The second payment must not touch the reserved coin.
	tx2, _, err := w.Send(n, []Payment{{To: to, Amount: 5 * chain.POWM}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if tx2.Inputs[0].PrevTxID == tx.Inputs[0].PrevTxID {
		t.Fatal("the second payment used the coin reserved by the first")
	}
	// The third payment: the confirmed coins are reserved, but our own unconfirmed change can be spent
	// right away — a hot wallet does not wait for a block (the node chains up to 25, like bitcoind).
	// Monero would stall here.
	tx3, _, err := w.Send(n, []Payment{{To: to, Amount: chain.POWM}}, 0)
	if err != nil {
		t.Fatalf("the third payment from the unconfirmed change: %v", err)
	}
	if p := tx3.Inputs[0].PrevTxID; p != tx.TxID() && p != tx2.TxID() {
		t.Fatal("the third payment does not spend the change from the previous ones")
	}

	n.Mine(nil, nil)
	mustSync(t, w, n)
	b = w.Balance(1)
	if b.Pending != 0 || b.Incoming != 0 {
		t.Fatalf("something is still pending after confirmation: %+v", b)
	}
	var fees int64
	for _, id := range [][32]byte{tx.TxID(), tx2.TxID(), tx3.TxID()} {
		v, ok := w.Transaction(id)
		if !ok || v.Confirmations != 1 {
			t.Fatalf("the transaction %x is not confirmed", id[:4])
		}
		fees += v.Fee
	}
	if want := 80*chain.POWM - 16*chain.POWM - fees; b.Spendable != want {
		t.Fatalf("the balance is %s, expected %s", FormatAmount(b.Spendable), FormatAmount(want))
	}
	// The recipient sees it.
	mustSync(t, other, n)
	if got := other.Balance(1).Spendable; got != 16*chain.POWM {
		t.Fatalf("the recipient has %s", FormatAmount(got))
	}
	if v, ok := w.Transaction(tx.TxID()); !ok || v.Confirmations != 1 || v.Net() != -(10*chain.POWM+fee) {
		t.Fatalf("the payment's history: %+v", v)
	}
}

// A reorg: the block with an incoming payment falls out → the coin disappears; our sent transaction
// comes back among the unconfirmed ones and its coins stay reserved.
func TestReorgRollsBack(t *testing.T) {
	w, n := fundedWallet(t)
	other, _ := testWallet(t)
	to := pubOf(t, other, must(other.NewAddress("")))
	me := pubOf(t, w, must(w.NewAddress("")))

	tx, _, err := w.Send(n, []Payment{{To: to, Amount: 10 * chain.POWM}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	forkAt := len(n.Chain)
	n.Mine(&me, nil) // a block with our payment and with another reward for us
	mustSync(t, w, n)
	if v, _ := w.Transaction(tx.TxID()); v.Confirmations != 1 {
		t.Fatal("the payment is not confirmed")
	}

	n.Fork(forkAt)
	n.Mine(nil, nil)
	n.Mine(nil, nil) // a longer branch without our payment and without our reward
	res := mustSync(t, w, n)
	if res.RolledBack != 1 {
		t.Fatalf("%d blocks rolled back, expected 1", res.RolledBack)
	}
	if v, ok := w.Transaction(tx.TxID()); !ok || v.Height != 0 {
		t.Fatalf("our payment after the reorg: %+v, %v", v, ok)
	}
	b := w.Balance(1)
	if b.Immature != 0 {
		t.Fatalf("the reward from the disconnected block stayed: %+v", b)
	}
	if b.Pending != 40*chain.POWM {
		t.Fatalf("the payment's coins are not reserved after the reorg: %+v", b)
	}
}

// A restore from the words finds coins even on an address that was handed out far ahead (an exchange),
// and moves the counter past it.
func TestRestoreFindsFarAddress(t *testing.T) {
	w, m := testWallet(t)
	n := wallettest.NewNode()
	var far string
	for i := 0; i < 600; i++ {
		far = must(w.NewAddress(""))
	}
	pk := pubOf(t, w, far)
	n.Mine(&pk, nil)

	r, err := Restore(filepath.Join(t.TempDir(), "restore"), "x", m, params, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	mustSync(t, r, n)
	if b := r.Balance(1); b.Immature != 40*chain.POWM {
		t.Fatalf("the restored wallet does not see the coin on the 600th address: %+v", b)
	}
	if st := r.Status(); st.Addresses != 600 {
		t.Fatalf("the address counter after the restore is %d, expected 600", st.Addresses)
	}
}

func TestImportAndDumpKey(t *testing.T) {
	src, n := fundedWallet(t)
	addr := src.Addresses()[0].Address
	secret, err := src.DumpPrivKey(addr)
	if err != nil {
		t.Fatal(err)
	}

	dst, _ := testWallet(t)
	mustSync(t, dst, n)
	got, rescan, err := dst.ImportPrivKey(secret, "import", 0)
	if err != nil || got != addr || !rescan {
		t.Fatalf("import: %s, rescan %v, %v", got, rescan, err)
	}
	if err := dst.Rescan(0); err != nil {
		t.Fatal(err)
	}
	mustSync(t, dst, n)
	if b := dst.Balance(1); b.Spendable != 80*chain.POWM {
		t.Fatalf("after the import and rescan: %+v", b)
	}
	if back, _ := dst.DumpPrivKey(addr); back != secret {
		t.Fatal("the dump of the imported key differs")
	}
	// The import survives closing.
	path := dst.Path()
	dst.Close()
	again, err := Open(path, "password", params)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if !again.IsMine(addr) || again.Label(addr) != "import" {
		t.Fatal("the imported key is missing after opening")
	}
	if _, _, err := again.ImportPrivKey(addr, "", 0); err == nil {
		t.Fatal("an address passed as a private key")
	}
}

// A transaction the node lost and refuses again (the inputs spent elsewhere) is dropped.
func TestPendingAbandonedWhenRejected(t *testing.T) {
	w, n := fundedWallet(t)
	to := pubOf(t, w, must(w.NewAddress("")))
	if _, _, err := w.Send(n, []Payment{{To: to, Amount: 10 * chain.POWM}}, 0); err != nil {
		t.Fatal(err)
	}
	old := PendingGrace
	PendingGrace = -time.Second
	defer func() { PendingGrace = old }()
	n.Pool = make(map[[32]byte]*chain.Tx) // the node restarted
	n.Reject = true
	mustSync(t, w, n)
	if b := w.Balance(1); b.Pending != 0 || b.Spendable != 80*chain.POWM {
		t.Fatalf("the refused transaction still blocks the coins: %+v", b)
	}
}

// When the node refuses a parent, the wallet drops the child that spent its unconfirmed change too.
func TestAbandonCascadesToChildren(t *testing.T) {
	w, n := fundedWallet(t)
	to := pubOf(t, w, must(w.NewAddress("")))
	parent, _, err := w.Send(n, []Payment{{To: to, Amount: 39 * chain.POWM}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	// the second confirmed coin (40) → the third payment has to come from the unconfirmed change
	if _, _, err := w.Send(n, []Payment{{To: to, Amount: 39 * chain.POWM}}, 0); err != nil {
		t.Fatal(err)
	}
	child, _, err := w.Send(n, []Payment{{To: to, Amount: 10 * chain.POWM}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	_ = parent // the child spends the change of one of the first two payments
	if child.Inputs[0].PrevTxID == ([32]byte{}) {
		t.Fatal("a child without an input")
	}
	old := PendingGrace
	PendingGrace = -time.Second
	defer func() { PendingGrace = old }()
	n.Pool = make(map[[32]byte]*chain.Tx)
	n.Reject = true
	mustSync(t, w, n)
	if len(w.Transactions()) != 2 { // only the two coinbase rewards remain
		t.Fatalf("%d transactions remained after the refusal, expected 2 (the rewards)", len(w.Transactions()))
	}
	if b := w.Balance(1); b.Pending != 0 || b.Incoming != 0 || b.Spendable != 80*chain.POWM {
		t.Fatalf("after dropping the whole chain: %+v", b)
	}
}

// A node that merely lost a transaction gets it again.
func TestPendingRebroadcast(t *testing.T) {
	w, n := fundedWallet(t)
	to := pubOf(t, w, must(w.NewAddress("")))
	tx, _, err := w.Send(n, []Payment{{To: to, Amount: 10 * chain.POWM}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	old := PendingGrace
	PendingGrace = -time.Second
	defer func() { PendingGrace = old }()
	n.Pool = make(map[[32]byte]*chain.Tx)
	mustSync(t, w, n)
	if _, ok := n.Pool[tx.TxID()]; !ok {
		t.Fatal("the lost transaction was not sent again")
	}
}

func TestAmounts(t *testing.T) {
	for in, want := range map[string]int64{"1": chain.POWM, "0.1": 10_000_000, ".5": 50_000_000,
		"0.00000001": 1, "12.3456789": 1_234_567_890, "0.999": 99_900_000} {
		got, err := ParseAmount(in)
		if err != nil || got != want {
			t.Errorf("ParseAmount(%q) = %d, %v; expected %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "-1", "1.", "0.000000001", "1e8", "1,5", "abc", "99999999999999"} {
		if _, err := ParseAmount(bad); err == nil {
			t.Errorf("ParseAmount(%q) passed", bad)
		}
	}
	if s := FormatAmount(-1_234_567_890); s != "-12.34567890" {
		t.Errorf("FormatAmount: %s", s)
	}
}

// A fixed change address (HOT_MAIN_WALLET): the change does not scatter across new addresses.
func TestFixedChangeAddress(t *testing.T) {
	w, n := fundedWallet(t)
	main := w.Addresses()[0].Address
	if err := w.SetChangeAddress("rpowm1qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqq"); err == nil {
		t.Fatal("a foreign/invalid address passed as a change address")
	}
	if err := w.SetChangeAddress(main); err != nil {
		t.Fatal(err)
	}
	if got, _ := w.ChangeAddress(); got != main {
		t.Fatalf("getrawchangeaddress %s, expected %s", got, main)
	}
	other, _ := testWallet(t)
	to := pubOf(t, other, must(other.NewAddress("")))
	for range 2 {
		tx, _, err := w.Send(n, []Payment{{To: to, Amount: chain.POWM}}, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(tx.Outputs) != 2 || w.encode(a32(tx.Outputs[1].Script)) != main {
			t.Fatalf("the change did not go to the main address")
		}
		if v, _ := w.Transaction(tx.TxID()); !v.Outputs[1].Change {
			t.Fatal("change to the fixed address is not marked as change (it would show up as a receive)")
		}
	}
}

// Collecting deposits as a bitcoind integration does: a deposit wallet, fundrawtransaction with the change straight
// to the hot wallet, signrawtransactionwithwallet, broadcast.
func TestFundRawAndSignRaw(t *testing.T) {
	dep, n := fundedWallet(t)
	hot, _ := testWallet(t)
	hotAddr := must(hot.NewAddress("hot main"))
	hotPub := pubOf(t, hot, hotAddr)

	coins := dep.Unspent(1, 1<<30)
	raw := &chain.Tx{Version: 1,
		Inputs:  []chain.TxIn{{PrevTxID: coins[0].Outpoint.TxID, PrevIndex: coins[0].Outpoint.Index}},
		Outputs: []chain.TxOut{{Value: 10 * chain.POWM, Script: hotPub[:]}}}
	funded, fee, pos, err := dep.FundRaw(raw, &hotPub, 0)
	if err != nil {
		t.Fatal(err)
	}
	if pos != 1 || len(funded.Witness) != 0 || fee <= 0 {
		t.Fatalf("fundraw: pos %d, witness %d, fee %d", pos, len(funded.Witness), fee)
	}
	if got := a32(funded.Outputs[pos].Script); got != hotPub {
		t.Fatal("the change does not go to the hot wallet")
	}
	signed, missing, err := dep.SignRaw(funded)
	if err != nil || len(missing) != 0 || len(signed.Witness) != len(signed.Inputs) {
		t.Fatalf("sign: %v, missing %v", err, missing)
	}
	// A foreign wallet does not sign.
	if _, miss, _ := hot.SignRaw(funded); len(miss) == 0 {
		t.Fatal("the hot wallet signed coins it does not have")
	}
	if err := n.Broadcast(signed); err != nil {
		t.Fatal(err)
	}
	if err := dep.Commit(signed); err != nil {
		t.Fatal(err)
	}
	n.Mine(nil, nil)
	mustSync(t, hot, n)
	if got := hot.Balance(1).Spendable; got != 40*chain.POWM-fee {
		t.Fatalf("the hot wallet has %s, expected the whole coin minus the fee", FormatAmount(got))
	}
}

// sweepall: everything confirmed to the hot wallet, in batches, the fee out of the output.
func TestSweep(t *testing.T) {
	dep, _ := testWallet(t)
	node := wallettest.NewNode()
	addr := must(dep.NewAddress(""))
	pk := pubOf(t, dep, addr)
	for range 5 {
		node.Mine(&pk, nil)
	}
	for range chain.CoinbaseMaturity {
		node.Mine(nil, nil)
	}
	mustSync(t, dep, node)
	hot, _ := testWallet(t)
	hotPub := pubOf(t, hot, must(hot.NewAddress("")))

	txs, err := dep.Sweep(node, hotPub, 1, 2, 0) // two inputs at a time → 3 transactions
	if err != nil {
		t.Fatal(err)
	}
	if len(txs) != 3 {
		t.Fatalf("%d sweeping transactions, expected 3", len(txs))
	}
	var fees int64
	for _, tx := range txs {
		if len(tx.Outputs) != 1 {
			t.Fatal("a sweeping transaction has change")
		}
		v, _ := dep.Transaction(tx.TxID())
		fees += v.Fee
	}
	node.Mine(nil, nil)
	mustSync(t, hot, node)
	mustSync(t, dep, node)
	if got := hot.Balance(1).Spendable; got != 200*chain.POWM-fees {
		t.Fatalf("the hot wallet has %s", FormatAmount(got))
	}
	if b := dep.Balance(0); b.Available() != 0 {
		t.Fatalf("the deposit wallet still holds %+v", b)
	}
}

// An immature reward does not show up in listunspent and fundraw refuses it as an input (found live:
// the node would refuse the transaction with "the coinbase has not matured yet").
func TestImmatureCoinsNotOffered(t *testing.T) {
	w, _ := testWallet(t)
	n := wallettest.NewNode()
	pk := pubOf(t, w, must(w.NewAddress("")))
	n.Mine(&pk, nil)
	mustSync(t, w, n)
	if got := w.Unspent(0, 1<<30); len(got) != 0 {
		t.Fatalf("listunspent shows an immature reward: %d", len(got))
	}
	cb := n.Chain[1].Txs[0].TxID()
	raw := &chain.Tx{Version: 1, Inputs: []chain.TxIn{{PrevTxID: cb}},
		Outputs: []chain.TxOut{{Value: chain.POWM, Script: pk[:]}}}
	if _, _, _, err := w.FundRaw(raw, nil, 0); !errors.Is(err, ErrNoFunds) {
		t.Fatalf("fundraw with an immature reward: %v", err)
	}
}

// What PlanSweep promises is what Sweep then does: the same number of transactions, the same coins, the
// same amount arriving and the same fee. A wallet shows the plan to a person before they type a password.
func TestPlanSweepMatchesSweep(t *testing.T) {
	for _, maxInputs := range []int{2, 3, 0} {
		dep, _ := testWallet(t)
		node := wallettest.NewNode()
		pk := pubOf(t, dep, must(dep.NewAddress("")))
		for range 5 {
			node.Mine(&pk, nil)
		}
		for range chain.CoinbaseMaturity {
			node.Mine(nil, nil)
		}
		mustSync(t, dep, node)
		hot, _ := testWallet(t)
		hotPub := pubOf(t, hot, must(hot.NewAddress("")))

		before := dep.Balance(1)
		plan, err := dep.PlanSweep(1, maxInputs, 0)
		if err != nil {
			t.Fatal(err)
		}
		if after := dep.Balance(1); after != before {
			t.Fatalf("planning changed the wallet: %+v → %+v", before, after)
		}
		txs, err := dep.Sweep(node, hotPub, 1, maxInputs, 0)
		if err != nil {
			t.Fatal(err)
		}
		var amount, fee int64
		inputs := 0
		for _, tx := range txs {
			v, _ := dep.Transaction(tx.TxID())
			fee += v.Fee
			amount += tx.Outputs[0].Value
			inputs += len(tx.Inputs)
		}
		got := SweepPlan{Transactions: len(txs), Inputs: inputs, Amount: amount, Fee: fee}
		if plan != got {
			t.Fatalf("maxInputs %d: planned %+v, swept %+v", maxInputs, plan, got)
		}
		if plan.Amount+plan.Fee != before.Spendable {
			t.Fatalf("the plan does not add up to the balance: %+v vs %d", plan, before.Spendable)
		}
	}
}

// With nothing to sweep the plan is empty, not an error.
func TestPlanSweepEmpty(t *testing.T) {
	w, _ := testWallet(t)
	plan, err := w.PlanSweep(1, 0, 0)
	if err != nil || plan != (SweepPlan{}) {
		t.Fatalf("%+v, %v", plan, err)
	}
}
