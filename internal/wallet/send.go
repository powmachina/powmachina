package wallet

import (
	"fmt"
	"math"
	"sort"
	"time"

	"powmachina/internal/chain"
)

// Payment is one recipient of a payment.
type Payment struct {
	To     [32]byte
	Amount int64
}

// Build assembles and signs a transaction. It does not reserve the coins yet — Commit does that after
// the node accepts the transaction (otherwise a refused transaction would block the coins).
//
// Input selection: confirmed first, largest first. It is optimal neither for privacy nor for the size
// of the UTXO set, but it is predictable — and with money predictability beats elegance.
func (w *Wallet) Build(pays []Payment, feePerKB int64) (*chain.Tx, int64, error) {
	outs, err := paymentOutputs(pays)
	if err != nil {
		return nil, 0, err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	change, err := w.changeKeyLocked(nil)
	if err != nil {
		return nil, 0, err
	}
	tx, _, fee, _, err := w.assembleLocked(outs, nil, true, change, feePerKB)
	return tx, fee, err
}

func paymentOutputs(pays []Payment) ([]chain.TxOut, error) {
	if len(pays) == 0 {
		return nil, fmt.Errorf("%w: no recipient", ErrAmountTooLow)
	}
	var amount int64
	outs := make([]chain.TxOut, 0, len(pays))
	for _, p := range pays {
		if p.Amount < chain.DustLimit {
			return nil, fmt.Errorf("%w: %d (minimum %d)", ErrAmountTooLow, p.Amount, chain.DustLimit)
		}
		if p.Amount > chain.MaxMoney || amount > chain.MaxMoney-p.Amount {
			return nil, fmt.Errorf("wallet: amount is unreasonably large")
		}
		amount += p.Amount
		outs = append(outs, chain.TxOut{Value: p.Amount, Script: append([]byte{}, p.To[:]...)})
	}
	return outs, nil
}

// isChangeLocked: an output is change when it goes to the change branch, or — in a transaction that
// spends our coins — to the configured fixed change address.
func (w *Wallet) isChangeLocked(ref keyRef, pub [32]byte, ours bool) bool {
	return ref.kind == branchChange || (ours && w.changeTo != nil && pub == *w.changeTo)
}

// SetChangeAddress sets a fixed address for change (the counterpart of HOT_MAIN_WALLET). Empty = every
// change goes to a new change address of the wallet. The address has to belong to the wallet — otherwise
// the change would leave. (A one-off to a foreign address can be sent through FundRaw, for instance when
// collecting deposits into a hot wallet.)
func (w *Wallet) SetChangeAddress(addr string) error {
	if addr == "" {
		w.mu.Lock()
		w.changeTo = nil
		w.mu.Unlock()
		return nil
	}
	pub, err := w.DecodeAddress(addr)
	if err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, ok := w.keys.secrets[pub]; !ok {
		return fmt.Errorf("wallet: change address %s does not belong to this wallet", addr)
	}
	w.changeTo = &pub
	return nil
}

// ChangeAddress returns the address the next change will go to (getrawchangeaddress).
func (w *Wallet) ChangeAddress() (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	pub, err := w.changeKeyLocked(nil)
	if err != nil {
		return "", err
	}
	return w.encode(pub), nil
}

func (w *Wallet) changeKeyLocked(override *[32]byte) ([32]byte, error) {
	switch {
	case override != nil:
		return *override, nil
	case w.changeTo != nil:
		return *w.changeTo, nil
	}
	return w.keys.pubAt(branchChange, w.nextChange)
}

// assembleLocked assembles and signs a transaction with the outputs `outs`. The inputs: `pre`
// (mandatory), and if `add`, it adds more from the spendable ones. The change goes to `change` unless it
// is dust. It returns the transaction, the coins used, the fee and the change's position (-1 = no change).
func (w *Wallet) assembleLocked(outs []chain.TxOut, pre []*Coin, add bool, change [32]byte,
	feePerKB int64) (*chain.Tx, []*Coin, int64, int, error) {
	if feePerKB < chain.MinRelayFeePerKB {
		feePerKB = chain.MinRelayFeePerKB
	}
	var amount int64
	for _, o := range outs {
		amount += o.Value
	}
	picked := append([]*Coin{}, pre...)
	var inSum int64
	used := make(map[chain.Outpoint]bool)
	for _, c := range pre {
		inSum += c.Value
		used[c.Outpoint] = true
	}
	if add {
		for _, c := range w.spendableLocked() {
			if inSum >= amount+feeFor(picked, outs, inSum-amount, feePerKB) && len(picked) > 0 {
				break
			}
			if used[c.Outpoint] {
				continue
			}
			picked = append(picked, c)
			inSum += c.Value
		}
	}
	if len(picked) == 0 {
		return nil, nil, 0, -1, ErrNoFunds
	}
	build := func(withChange bool, fee int64) (*chain.Tx, int) {
		tx := &chain.Tx{Version: 1}
		for _, c := range picked {
			tx.Inputs = append(tx.Inputs, chain.TxIn{PrevTxID: c.Outpoint.TxID, PrevIndex: c.Outpoint.Index})
		}
		for _, o := range outs {
			tx.Outputs = append(tx.Outputs, chain.TxOut{Value: o.Value, ScriptVersion: o.ScriptVersion,
				Script: append([]byte{}, o.Script...)})
		}
		pos := -1
		if withChange {
			pos = len(tx.Outputs)
			tx.Outputs = append(tx.Outputs, chain.TxOut{Value: inSum - amount - fee, Script: append([]byte{}, change[:]...)})
		}
		return tx, pos
	}
	// The fee depends on the change (a small change = more weight) and the change on the fee. A few
	// rounds are enough: the fee only grows in each round, so it terminates.
	fee := feeFor(picked, outs, inSum-amount, feePerKB)
	var tx *chain.Tx
	pos := -1
	for range 4 {
		if inSum < amount+fee {
			return nil, nil, 0, -1, fmt.Errorf("%w: available %s, needed %s (including fee %s)",
				ErrNoFunds, FormatAmount(inSum), FormatAmount(amount+fee), FormatAmount(fee))
		}
		// A tiny remainder is not worth sending — it is added to the fee rather than to the UTXO set.
		tx, pos = build(inSum-amount-fee >= chain.DustLimit, fee)
		if err := w.signLocked(tx, picked); err != nil {
			return nil, nil, 0, -1, err
		}
		need := txFee(tx, picked, feePerKB)
		if need <= fee {
			break
		}
		fee = need
	}
	if txFee(tx, picked, feePerKB) > fee {
		return nil, nil, 0, -1, fmt.Errorf("%w: fee did not converge", ErrNoFunds)
	}
	if chain.TxStorageMass(inValues(picked), tx) > chain.MaxBlockStorageMass-chain.CoinbaseStorageMassReserve {
		return nil, nil, 0, -1, fmt.Errorf("%w: amount is too small for the network", ErrAmountTooLow)
	}
	var outSum int64
	for _, o := range tx.Outputs {
		outSum += o.Value
	}
	return tx, picked, inSum - outSum, pos, nil
}

// FundRaw adds inputs and change to an unsigned transaction (fundrawtransaction). The inputs it already
// has have to be the wallet's coins. `changeTo` nil = the wallet's default change; it may also be a
// foreign address (collecting deposits: the change straight to the hot wallet). It returns the unsigned transaction.
func (w *Wallet) FundRaw(tx *chain.Tx, changeTo *[32]byte, feePerKB int64) (*chain.Tx, int64, int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	var pre []*Coin
	for _, in := range tx.Inputs {
		c, ok := w.coins[chain.Outpoint{TxID: in.PrevTxID, Index: in.PrevIndex}]
		switch {
		case !ok || c.spent():
			return nil, 0, -1, fmt.Errorf("%w: input %x:%d is not a coin of this wallet", ErrUnknownKey, in.PrevTxID[:8], in.PrevIndex)
		case !w.matureLocked(c):
			return nil, 0, -1, fmt.Errorf("%w: input %x:%d is an immature mining reward", ErrNoFunds, in.PrevTxID[:8], in.PrevIndex)
		case c.pending():
			return nil, 0, -1, fmt.Errorf("%w: input %x:%d is already spent by a pending transaction", ErrNoFunds, in.PrevTxID[:8], in.PrevIndex)
		}
		pre = append(pre, c)
	}
	change, err := w.changeKeyLocked(changeTo)
	if err != nil {
		return nil, 0, -1, err
	}
	funded, _, fee, pos, err := w.assembleLocked(tx.Outputs, pre, true, change, feePerKB)
	if err != nil {
		return nil, 0, -1, err
	}
	funded.Witness = nil
	return funded, fee, pos, nil
}

// SignRaw signs a transaction's inputs with the wallet's coins (signrawtransactionwithwallet). The
// signature commits to the amounts of every input, so every input has to be the wallet's coin —
// otherwise it returns the unsigned transaction and the list of inputs it does not know.
func (w *Wallet) SignRaw(tx *chain.Tx) (*chain.Tx, []string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	var picked []*Coin
	var missing []string
	for _, in := range tx.Inputs {
		c, ok := w.coins[chain.Outpoint{TxID: in.PrevTxID, Index: in.PrevIndex}]
		if !ok {
			missing = append(missing, fmt.Sprintf("%x:%d", in.PrevTxID, in.PrevIndex))
			continue
		}
		if _, have := w.keys.secrets[c.PubKey]; !have {
			missing = append(missing, fmt.Sprintf("%x:%d", in.PrevTxID, in.PrevIndex))
			continue
		}
		picked = append(picked, c)
	}
	out := *tx
	out.Inputs = append([]chain.TxIn{}, tx.Inputs...)
	out.Outputs = append([]chain.TxOut{}, tx.Outputs...)
	if len(missing) > 0 {
		out.Witness = nil
		return &out, missing, nil
	}
	if err := w.signLocked(&out, picked); err != nil {
		return nil, nil, err
	}
	return &out, nil, nil
}

// SweepMaxInputs is the cap on the inputs of one sweeping transaction. 500 inputs are ~54 kB — a quarter
// of a block; the storage mass of a sweep (many → one) is zero, only the bytes are paid for.
const SweepMaxInputs = 500

// Sweep sends every confirmed coin (≥ minConf) to `to`, at most `maxInputs` in one transaction, with the
// fee deducted from the output. It broadcasts and records every transaction; on an error it returns the
// ones that already went out.
func (w *Wallet) Sweep(node Node, to [32]byte, minConf uint64, maxInputs int, feePerKB int64) ([]*chain.Tx, error) {
	if maxInputs <= 0 || maxInputs > SweepMaxInputs {
		maxInputs = SweepMaxInputs
	}
	if feePerKB < chain.MinRelayFeePerKB {
		feePerKB = chain.MinRelayFeePerKB
	}
	var sent []*chain.Tx
	for {
		tx, err := w.sweepChunk(to, max(minConf, 1), maxInputs, feePerKB)
		if err != nil || tx == nil {
			return sent, err
		}
		if err := node.Broadcast(tx); err != nil {
			return sent, err
		}
		if err := w.Commit(tx); err != nil {
			return sent, err
		}
		sent = append(sent, tx)
	}
}

func (w *Wallet) sweepChunk(to [32]byte, minConf uint64, maxInputs int, feePerKB int64) (*chain.Tx, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	var picked []*Coin
	var sum int64
	for _, c := range w.spendableLocked() {
		if c.Height == 0 || w.confLocked(c.Height) < minConf {
			continue
		}
		picked = append(picked, c)
		sum += c.Value
		if len(picked) == maxInputs {
			break
		}
	}
	if len(picked) == 0 {
		return nil, nil
	}
	out := []chain.TxOut{{Value: sum, Script: append([]byte{}, to[:]...)}}
	fee := feeFor(picked, out, 0, feePerKB)
	var tx *chain.Tx
	for range 4 {
		if sum-fee < chain.DustLimit {
			return nil, fmt.Errorf("%w: coins to sweep do not cover the fee", ErrAmountTooLow)
		}
		tx = &chain.Tx{Version: 1}
		for _, c := range picked {
			tx.Inputs = append(tx.Inputs, chain.TxIn{PrevTxID: c.Outpoint.TxID, PrevIndex: c.Outpoint.Index})
		}
		tx.Outputs = []chain.TxOut{{Value: sum - fee, Script: append([]byte{}, to[:]...)}}
		if err := w.signLocked(tx, picked); err != nil {
			return nil, err
		}
		need := txFee(tx, picked, feePerKB)
		if need <= fee {
			return tx, nil
		}
		fee = need
	}
	return nil, fmt.Errorf("%w: fee did not converge", ErrNoFunds)
}

// SweepPlan is what Sweep would do right now.
type SweepPlan struct {
	Transactions int   // how many transactions it takes
	Inputs       int   // how many coins are swept
	Amount       int64 // what arrives at the destination
	Fee          int64 // what the network takes
}

// PlanSweep says what Sweep would do, without signing, broadcasting or recording anything — so that a
// person can be shown what "send everything" means before they type a password.
//
// It walks the same coins in the same order and takes the fee sweepChunk starts from. sweepChunk only
// raises that fee when the signed transaction turns out heavier than the estimate, which the estimate is
// built not to allow (it counts one output more than a sweep has). TestPlanSweepMatchesSweep holds the
// two together: a plan that drifts from what is sent would be a promise the wallet does not keep.
func (w *Wallet) PlanSweep(minConf uint64, maxInputs int, feePerKB int64) (SweepPlan, error) {
	if maxInputs <= 0 || maxInputs > SweepMaxInputs {
		maxInputs = SweepMaxInputs
	}
	if feePerKB < chain.MinRelayFeePerKB {
		feePerKB = chain.MinRelayFeePerKB
	}
	minConf = max(minConf, 1)
	w.mu.Lock()
	defer w.mu.Unlock()
	var plan SweepPlan
	var picked []*Coin
	var sum int64
	chunk := func() error {
		if len(picked) == 0 {
			return nil
		}
		fee := feeFor(picked, []chain.TxOut{{Value: sum, Script: make([]byte, chain.PubkeyBytes)}}, 0, feePerKB)
		if sum-fee < chain.DustLimit {
			return fmt.Errorf("%w: coins to sweep do not cover the fee", ErrAmountTooLow)
		}
		plan.Transactions++
		plan.Inputs += len(picked)
		plan.Amount += sum - fee
		plan.Fee += fee
		picked, sum = nil, 0
		return nil
	}
	for _, c := range w.spendableLocked() {
		if c.Height == 0 || w.confLocked(c.Height) < minConf {
			continue
		}
		picked = append(picked, c)
		sum += c.Value
		if len(picked) == maxInputs {
			if err := chunk(); err != nil {
				return plan, err
			}
		}
	}
	return plan, chunk()
}

// Commit records a sent transaction: its inputs are reserved until a block confirms it, and the change
// address is marked as used.
func (w *Wallet) Commit(tx *chain.Tx) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	txid := tx.TxID()
	if _, known := w.txs[txid]; known {
		return nil // already recorded (for instance sendrawtransaction of the same transaction again)
	}
	var spent, outSum int64
	var inputs []chain.Outpoint
	for _, in := range tx.Inputs {
		op := chain.Outpoint{TxID: in.PrevTxID, Index: in.PrevIndex}
		c, ok := w.coins[op]
		if !ok || c.spent() {
			return fmt.Errorf("wallet: transaction spends a coin the wallet does not have")
		}
		inputs = append(inputs, op)
		spent += c.Value
	}
	for _, op := range inputs {
		w.coins[op].PendingBy = txid
	}
	var received int64
	var outs []Output
	for i, o := range tx.Outputs {
		outSum += o.Value
		out := Output{Index: uint32(i), Value: o.Value}
		if len(o.Script) == chain.PubkeyBytes {
			pub := a32(o.Script)
			out.Address = w.encode(pub)
			if ref, ok := w.keys.derived[pub]; ok {
				out.Mine = true
				out.Change = w.isChangeLocked(ref, pub, spent > 0)
				received += o.Value
				w.markUsedLocked(ref)
				// Our own output can be spent right away, even before confirmation (the node chains up to 25).
				op := chain.Outpoint{TxID: txid, Index: uint32(i)}
				w.coins[op] = &Coin{Outpoint: op, Value: o.Value, PubKey: pub}
			}
		}
		outs = append(outs, out)
	}
	w.txs[txid] = &Tx{ID: txid, Time: time.Now().Unix(), Received: received, Spent: spent,
		Fee: spent - outSum, Outputs: outs, Raw: tx.Serialize(), Inputs: inputs}
	return w.saveLocked()
}

// Send = Build + sending through the node + Commit.
func (w *Wallet) Send(node Node, pays []Payment, feePerKB int64) (*chain.Tx, int64, error) {
	tx, fee, err := w.Build(pays, feePerKB)
	if err != nil {
		return nil, 0, err
	}
	if err := node.Broadcast(tx); err != nil {
		return nil, 0, err
	}
	return tx, fee, w.Commit(tx)
}

// spendableLocked: unspent, unreserved, matured coins including our own unconfirmed change. Confirmed
// first (chains of unconfirmed ones stay short — the node takes at most 25), and within that largest
// first.
func (w *Wallet) spendableLocked() []*Coin {
	var out []*Coin
	for _, c := range w.coins {
		if c.spent() || c.pending() || !w.matureLocked(c) {
			continue
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		if ci, cj := out[i].Height != 0, out[j].Height != 0; ci != cj {
			return ci
		}
		if out[i].Value != out[j].Value {
			return out[i].Value > out[j].Value
		}
		return string(out[i].Outpoint.TxID[:]) < string(out[j].Outpoint.TxID[:])
	})
	return out
}

func (w *Wallet) matureLocked(c *Coin) bool {
	return !c.Coinbase || w.height >= c.Height+chain.CoinbaseMaturity
}

func (w *Wallet) signLocked(tx *chain.Tx, picked []*Coin) error {
	coins := make([]chain.Coin, len(picked))
	for i, c := range picked {
		coins[i] = chain.Coin{Value: c.Value, Script: append([]byte{}, c.PubKey[:]...), Height: c.Height, Coinbase: c.Coinbase}
	}
	tx.Witness = make([]chain.Witness, len(picked))
	for i, c := range picked {
		sk, ok := w.keys.secrets[c.PubKey]
		if !ok {
			return ErrUnknownKey
		}
		sig, err := chain.SignInput(tx, i, coins, sk)
		if err != nil {
			return err
		}
		tx.Witness[i] = chain.Witness{Items: [][]byte{sig}}
	}
	return nil
}

func inValues(picked []*Coin) []int64 {
	v := make([]int64, len(picked))
	for i, c := range picked {
		v[i] = c.Value
	}
	return v
}

// feeFor estimates the fee before a transaction is assembled (the size is an estimate, the weight exact).
func feeFor(picked []*Coin, base []chain.TxOut, change int64, feePerKB int64) int64 {
	size := len(picked)*107 + (len(base)+1)*43 + 20
	outs := append([]chain.TxOut{}, base...)
	if change >= chain.DustLimit {
		outs = append(outs, chain.TxOut{Value: change, Script: make([]byte, 32)})
	}
	return feeOf(chain.PolicyWeight(size, inValues(picked), outs), feePerKB)
}

// txFee is the fee the mempool requires for a finished transaction.
func txFee(tx *chain.Tx, picked []*Coin, feePerKB int64) int64 {
	return feeOf(chain.PolicyWeight(len(tx.Serialize()), inValues(picked), tx.Outputs), feePerKB)
}

// feeOf rounds up — the mempool compares fee×1000/weight ≥ the rate.
func feeOf(weight uint64, feePerKB int64) int64 {
	if weight > math.MaxInt64/uint64(feePerKB) {
		return math.MaxInt64 / 2
	}
	return max((int64(weight)*feePerKB+999)/1000, 1)
}
