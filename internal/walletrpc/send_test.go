package walletrpc

import (
	"encoding/hex"
	"encoding/json"
	"testing"

	"powmachina/internal/chain"
)

// paidTo sums what the chain and the mempool of the test node pay to a key.
func (e *env) paidTo(pk [32]byte) int64 {
	var sum int64
	count := func(tx *chain.Tx) {
		for _, o := range tx.Outputs {
			if string(o.Script) == string(pk[:]) {
				sum += o.Value
			}
		}
	}
	for _, b := range e.node.Chain {
		for _, tx := range b.Txs {
			count(tx)
		}
	}
	for _, tx := range e.node.Pool {
		count(tx)
	}
	return sum
}

// The node took the withdrawal and its reply got lost. The caller must not be told "error": it would
// repeat the withdrawal and the wallet would pay it again from other coins. It gets the transaction's
// ID, and the wallet answers for that ID from then on.
func TestSendToAddressAnswersWithTheTransactionWhenTheReplyIsLost(t *testing.T) {
	for _, method := range []string{"sendtoaddress", "sendmany"} {
		t.Run(method, func(t *testing.T) {
			e := setup(t)
			to := [32]byte{7, 7, 7}
			dest := encodeAddr(t, e, to)

			e.node.LoseReply = true
			var r reply
			if method == "sendtoaddress" {
				r = e.call(method, dest, "5")
			} else {
				r = e.call(method, "", map[string]string{dest: "5"})
			}
			e.node.LoseReply = false
			if r.Error != nil {
				t.Fatalf("%s reported a failure for a payment that is on its way: %+v", method, r.Error)
			}
			var txid string
			json.Unmarshal(r.Result, &txid)
			if b, err := hex.DecodeString(txid); err != nil || len(b) != 32 {
				t.Fatalf("%s answered %s, expected a transaction ID", method, r.Result)
			}
			var got struct {
				Confirmations uint64 `json:"confirmations"`
			}
			json.Unmarshal(e.ok("gettransaction", txid), &got)
			if got.Confirmations != 0 {
				t.Fatalf("gettransaction before a block: %+v", got)
			}
			e.mine()
			json.Unmarshal(e.ok("gettransaction", txid), &got)
			if got.Confirmations != 1 {
				t.Fatalf("gettransaction after a block: %+v", got)
			}
			if paid := e.paidTo(to); paid != 5*chain.POWM {
				t.Fatalf("the recipient was paid %d for one payment of 5 POWM", paid)
			}
		})
	}
}

func encodeAddr(t *testing.T, e *env, pk [32]byte) string {
	t.Helper()
	var decoded struct {
		Vout []struct {
			ScriptPubKey struct {
				Address string `json:"address"`
			} `json:"scriptPubKey"`
		} `json:"vout"`
	}
	raw := (&chain.Tx{Version: 1, Outputs: []chain.TxOut{{Value: chain.POWM, Script: pk[:]}}}).Serialize()
	json.Unmarshal(e.ok("decoderawtransaction", hex.EncodeToString(raw)), &decoded)
	if len(decoded.Vout) != 1 || decoded.Vout[0].ScriptPubKey.Address == "" {
		t.Fatalf("cannot encode an address: %+v", decoded)
	}
	return decoded.Vout[0].ScriptPubKey.Address
}

// The raw path the same: a transaction of the wallet whose broadcast got no answer stays reserved, so
// the next fundrawtransaction cannot build a second one from its coins.
func TestSendRawTransactionKeepsTheCoinsWhenTheReplyIsLost(t *testing.T) {
	e := setup(t)
	dest := encodeAddr(t, e, [32]byte{9})
	var funded struct {
		Hex string `json:"hex"`
	}
	json.Unmarshal(e.ok("createrawtransaction", []any{}, map[string]string{dest: "79"}), &funded.Hex)
	json.Unmarshal(e.ok("fundrawtransaction", funded.Hex), &funded)
	var signed struct {
		Hex string `json:"hex"`
	}
	json.Unmarshal(e.ok("signrawtransactionwithwallet", funded.Hex), &signed)

	e.node.LoseReply = true
	r := e.call("sendrawtransaction", signed.Hex)
	e.node.LoseReply = false
	if r.Error != nil {
		t.Fatalf("sendrawtransaction reported a failure for a transaction that is on its way: %+v", r.Error)
	}
	// Both rewards are in that transaction; a second one of the same size has nothing to take.
	var again string
	json.Unmarshal(e.ok("createrawtransaction", []any{}, map[string]string{dest: "79"}), &again)
	if r := e.call("fundrawtransaction", again); r.Error == nil || r.Error.Code != codeInsufficient {
		t.Fatalf("the coins of the first transaction were offered again: %+v %s", r.Error, r.Result)
	}
}
