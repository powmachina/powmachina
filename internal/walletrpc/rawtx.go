package walletrpc

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"powmachina/internal/address"
	"powmachina/internal/chain"
	"powmachina/internal/wallet"
)

// Raw transactions — the path a bitcoind integration uses to collect deposits and verify change:
// createrawtransaction → fundrawtransaction {changeAddress} → signrawtransactionwithwallet →
// (decoderawtransaction) → sendrawtransaction.

func (s *Server) walletless(ctx *walletCtx, method string, a args) (any, error) {
	switch method {
	case "help":
		return s.help(), nil
	case "listwallets":
		return s.names(), nil
	case "createrawtransaction":
		return s.createRaw(a)
	case "decoderawtransaction":
		tx, err := s.rawArg(a, 0)
		if err != nil {
			return nil, err
		}
		return s.decode(tx), nil
	case "sendrawtransaction":
		return s.sendRaw(ctx, a)
	}
	return nil, &rpcError{codeMethodNotFound, "Method not found: " + method}
}

func (s *Server) decodeAddr(addr string) ([32]byte, error) {
	v, prog, err := address.Decode(s.params().Bech32HRP, addr)
	if err != nil || v != chain.ScriptVersionBasic || len(prog) != chain.PubkeyBytes {
		return [32]byte{}, &rpcError{codeInvalidAddress, "Invalid address: " + addr}
	}
	var pk [32]byte
	copy(pk[:], prog)
	return pk, nil
}

func (s *Server) rawArg(a args, i int) (*chain.Tx, error) {
	h, err := a.str(i, "")
	if err != nil {
		return nil, err
	}
	b, err := hex.DecodeString(h)
	if err != nil {
		return nil, &rpcError{codeDeserialize, "TX decode failed: not hex"}
	}
	tx, used, err := chain.DeserializeTx(b)
	if err != nil || used != len(b) {
		return nil, &rpcError{codeDeserialize, "TX decode failed"}
	}
	return tx, nil
}

func (s *Server) createRaw(a args) (any, error) {
	if !a.present(1) {
		return nil, &rpcError{codeInvalidParam, "inputs and outputs are required"}
	}
	var ins []struct {
		TxID     string  `json:"txid"`
		Vout     *uint32 `json:"vout"`
		Sequence *uint32 `json:"sequence"`
	}
	if a.present(0) {
		if err := json.Unmarshal(a[0], &ins); err != nil {
			return nil, &rpcError{codeInvalidParam, "inputs must be an array of {txid, vout}"}
		}
	}
	tx := &chain.Tx{Version: 1}
	for _, in := range ins {
		b, err := hex.DecodeString(in.TxID)
		if err != nil || len(b) != 32 || in.Vout == nil {
			return nil, &rpcError{codeInvalidParam, "input needs txid (64 hex) and vout"}
		}
		ti := chain.TxIn{PrevIndex: *in.Vout}
		copy(ti.PrevTxID[:], b)
		if in.Sequence != nil {
			ti.Sequence = *in.Sequence
		}
		tx.Inputs = append(tx.Inputs, ti)
	}
	// The outputs: an object {address: amount} or an array [{address: amount}, …] (an array allows the
	// same address several times — only some bitcoin daemons can do that).
	var objs []map[string]json.RawMessage
	t := strings.TrimSpace(string(a[1]))
	if strings.HasPrefix(t, "[") {
		if err := json.Unmarshal(a[1], &objs); err != nil {
			return nil, &rpcError{codeInvalidParam, "outputs must be {address: amount} or [{address: amount}]"}
		}
	} else {
		var one map[string]json.RawMessage
		if err := json.Unmarshal(a[1], &one); err != nil {
			return nil, &rpcError{codeInvalidParam, "outputs must be {address: amount} or [{address: amount}]"}
		}
		for _, k := range sortedKeys(one) {
			objs = append(objs, map[string]json.RawMessage{k: one[k]})
		}
	}
	for _, o := range objs {
		for addr, raw := range o {
			if addr == "data" {
				return nil, &rpcError{codeInvalidParam, "data outputs are not supported"}
			}
			pk, err := s.decodeAddr(addr)
			if err != nil {
				return nil, err
			}
			v, err := amountOf(raw)
			if err != nil {
				return nil, err
			}
			tx.Outputs = append(tx.Outputs, chain.TxOut{Value: v, Script: pk[:]})
		}
	}
	if lt, err := a.uint(2, 0); err != nil {
		return nil, err
	} else {
		tx.LockTime = uint32(lt)
	}
	return hex.EncodeToString(tx.Serialize()), nil
}

func (s *Server) decode(tx *chain.Tx) map[string]any {
	id := tx.TxID()
	vin := make([]map[string]any, 0, len(tx.Inputs))
	for _, in := range tx.Inputs {
		vin = append(vin, map[string]any{"txid": hex.EncodeToString(in.PrevTxID[:]), "vout": in.PrevIndex,
			"sequence": in.Sequence})
	}
	vout := make([]map[string]any, 0, len(tx.Outputs))
	for i, o := range tx.Outputs {
		spk := map[string]any{"hex": hex.EncodeToString(append([]byte{o.ScriptVersion}, o.Script...))}
		if o.ScriptVersion == chain.ScriptVersionBasic && len(o.Script) == chain.PubkeyBytes {
			a, _ := address.Encode(s.params().Bech32HRP, o.ScriptVersion, o.Script)
			spk["address"] = a
			spk["type"] = "witness_v0_schnorr"
		} else if o.ScriptVersion == chain.ScriptVersionData {
			spk["type"] = "nulldata"
		} else {
			spk["type"] = "nonstandard"
		}
		vout = append(vout, map[string]any{"value": amt(o.Value), "n": i, "scriptPubKey": spk})
	}
	return map[string]any{"txid": hex.EncodeToString(id[:]), "hash": hex.EncodeToString(id[:]),
		"version": tx.Version, "size": len(tx.Serialize()), "locktime": tx.LockTime, "vin": vin, "vout": vout}
}

// sendRaw broadcasts a transaction through the node and records it in the wallet whose coins it spends —
// that one reserves them and the next fundrawtransaction does not offer them again.
func (s *Server) sendRaw(ctx *walletCtx, a args) (any, error) {
	tx, err := s.rawArg(a, 0)
	if err != nil {
		return nil, err
	}
	if len(tx.Witness) != len(tx.Inputs) {
		return nil, &rpcError{codeVerifyRejected, "transaction is not signed"}
	}
	if err := s.node.Broadcast(tx); err != nil {
		return nil, &rpcError{codeVerifyRejected, err.Error()}
	}
	// The transaction is recorded by the wallet it belongs to. The list is a copy — wallets may be added
	// or removed in the meantime (a GUI with `-allow-manage`).
	targets := s.names()
	if ctx != nil {
		targets = append([]string{ctx.name}, targets...)
	}
	for _, name := range targets {
		l, _, ok := s.lookup(name)
		if ok && l.w.Commit(tx) == nil {
			break
		}
	}
	id := tx.TxID()
	return hex.EncodeToString(id[:]), nil
}

func (s *walletCtx) fundRaw(a args) (any, error) {
	tx, err := s.rawArg(a, 0)
	if err != nil {
		return nil, err
	}
	var opts struct {
		ChangeAddress  string          `json:"changeAddress"`
		ChangePosition *int            `json:"changePosition"`
		FeeRate        json.RawMessage `json:"feeRate"`
		SubtractFee    []int           `json:"subtractFeeFromOutputs"`
		LockUnspents   bool            `json:"lockUnspents"`
		IncludeWatch   bool            `json:"includeWatching"`
	}
	if a.present(1) {
		if err := json.Unmarshal(a[1], &opts); err != nil {
			return nil, &rpcError{codeInvalidParam, "options: " + err.Error()}
		}
	}
	if len(opts.SubtractFee) > 0 {
		return nil, &rpcError{codeInvalidParam, "subtractFeeFromOutputs is not supported"}
	}
	var change *[32]byte
	if opts.ChangeAddress != "" {
		pk, err := s.decodeAddr(opts.ChangeAddress)
		if err != nil {
			return nil, err
		}
		change = &pk
	}
	feePerKB := s.FeePerKB
	if len(opts.FeeRate) > 0 {
		// feeRate as in bitcoind: POWM per 1 000 units (of weight, in our case)
		v, err := amountOf(opts.FeeRate)
		if err != nil {
			return nil, err
		}
		feePerKB = v
	}
	funded, fee, pos, err := s.w.FundRaw(tx, change, feePerKB)
	if err != nil {
		return nil, err
	}
	if opts.ChangePosition != nil && pos >= 0 {
		want := *opts.ChangePosition
		if want < 0 || want >= len(funded.Outputs) {
			return nil, &rpcError{codeInvalidParam, "changePosition out of bounds"}
		}
		ch := funded.Outputs[pos]
		rest := append(append([]chain.TxOut{}, funded.Outputs[:pos]...), funded.Outputs[pos+1:]...)
		funded.Outputs = append(append(append([]chain.TxOut{}, rest[:want]...), ch), rest[want:]...)
		pos = want
	}
	return map[string]any{"hex": hex.EncodeToString(funded.Serialize()), "fee": amt(fee), "changepos": pos}, nil
}

func (s *walletCtx) signRaw(a args) (any, error) {
	if err := s.needUnlocked(); err != nil {
		return nil, err
	}
	tx, err := s.rawArg(a, 0)
	if err != nil {
		return nil, err
	}
	signed, missing, err := s.w.SignRaw(tx)
	if err != nil {
		return nil, err
	}
	out := map[string]any{"hex": hex.EncodeToString(signed.Serialize()), "complete": len(missing) == 0}
	if len(missing) > 0 {
		var errs []map[string]any
		for _, m := range missing {
			txid, vout, _ := strings.Cut(m, ":")
			errs = append(errs, map[string]any{"txid": txid, "vout": vout,
				"error": "Input not found or already spent in this wallet"})
		}
		out["errors"] = errs
	}
	return out, nil
}

func (s *walletCtx) sweepAll(a args) (any, error) {
	addr, err := a.str(0, "")
	if err != nil {
		return nil, err
	}
	to, err := s.decodeAddr(addr)
	if err != nil {
		return nil, err
	}
	minconf, err := a.uint(1, 1)
	if err != nil {
		return nil, err
	}
	maxIn, err := a.uint(2, wallet.SweepMaxInputs)
	if err != nil {
		return nil, err
	}
	// estimate: say what the sweep would do and do nothing. It needs no password — it signs nothing and
	// moves nothing — which is the point: the numbers can be shown before the password is asked for.
	estimate, err := a.boolean(3, false)
	if err != nil {
		return nil, err
	}
	if estimate {
		plan, err := s.w.PlanSweep(minconf, int(maxIn), s.FeePerKB)
		if err != nil {
			return nil, err
		}
		return map[string]any{"estimate": true, "amount": amt(plan.Amount), "fee": amt(plan.Fee),
			"transactions": plan.Transactions, "inputs": plan.Inputs}, nil
	}
	if err := s.needUnlocked(); err != nil {
		return nil, err
	}
	s.st.sendMu.Lock()
	defer s.st.sendMu.Unlock()
	txs, err := s.w.Sweep(s.node, to, minconf, int(maxIn), s.FeePerKB)
	ids := []string{}
	var total int64
	for _, tx := range txs {
		id := tx.TxID()
		ids = append(ids, hex.EncodeToString(id[:]))
		total += tx.Outputs[0].Value
	}
	if err != nil && len(txs) == 0 {
		return nil, err
	}
	out := map[string]any{"txids": ids, "amount": amt(total)}
	if err != nil {
		out["error"] = err.Error() // part of it went out, the rest did not
	}
	return out, nil
}

// walletPassphrase unlocks signing for `timeout` seconds. The first check costs an Argon2id (~1 s), the
// later ones are compared against a fingerprint of the password in memory.
func (s *walletCtx) walletPassphrase(a args) (any, error) {
	pass, err := a.str(0, "")
	if err != nil {
		return nil, err
	}
	timeout, err := a.uint(1, 0)
	if err != nil {
		return nil, err
	}
	if timeout == 0 || timeout > 100_000_000 {
		return nil, &rpcError{codeInvalidParam, "timeout must be 1–100000000 seconds"}
	}
	if !s.checkPass(pass) {
		return nil, &rpcError{codeWrongPassphrase, "Error: The wallet passphrase entered was incorrect."}
	}
	s.st.mu.Lock()
	s.st.until = time.Now().Add(time.Duration(timeout) * time.Second)
	s.st.mu.Unlock()
	return nil, nil
}

func (s *walletCtx) checkPass(pass string) bool {
	h := sha256.Sum256([]byte(pass))
	s.st.mu.Lock()
	known, want := s.st.passKnown, s.st.passHash
	s.st.mu.Unlock()
	if known {
		return subtle.ConstantTimeCompare(h[:], want[:]) == 1
	}
	if !s.w.CheckPassword(pass) {
		return false
	}
	s.st.mu.Lock()
	s.st.passHash, s.st.passKnown = h, true
	s.st.mu.Unlock()
	return true
}

func (s *walletCtx) passphraseChange(a args) (any, error) {
	old, err := a.str(0, "")
	if err != nil {
		return nil, err
	}
	nw, err := a.str(1, "")
	if err != nil {
		return nil, err
	}
	if err := s.w.ChangePassword(old, nw); err != nil {
		if errors.Is(err, wallet.ErrWrongPassword) {
			return nil, &rpcError{codeWrongPassphrase, "Error: The wallet passphrase entered was incorrect."}
		}
		return nil, err
	}
	s.st.mu.Lock()
	s.st.passKnown = false
	s.st.until = time.Time{}
	s.st.mu.Unlock()
	return nil, nil
}

func sortedKeys(m map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
