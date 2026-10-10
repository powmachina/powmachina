package chain

import (
	"errors"
	"math"
	"testing"
)

func outs(values ...int64) []TxOut {
	o := make([]TxOut, len(values))
	for i, v := range values {
		o[i] = TxOut{Value: v, Script: make([]byte, 32)}
	}
	return o
}

func repeat(v int64, n int) []int64 {
	out := make([]int64, n)
	for i := range out {
		out[i] = v
	}
	return out
}

// An ordinary payment has a negligible mass compared to its bytes — it costs people nothing extra.
func TestStorageMassNormalPaymentIsNegligible(t *testing.T) {
	m := StorageMass([]int64{50 * POWM}, outs(10*POWM, 40*POWM-1000), StorageMassC)
	if m > 10 {
		t.Fatalf("a payment of 10 POWM has mass %d, expected a negligible one", m)
	}
}

// Consolidating coins has zero mass — whoever moves many coins pays only for the bytes.
func TestStorageMassConsolidationIsFree(t *testing.T) {
	ins := repeat(38*POWM, 1000)
	if m := StorageMass(ins, outs(38_000*POWM-10_000), StorageMassC); m != 0 {
		t.Fatalf("consolidating 1 000 rewards has mass %d, expected 0", m)
	}
	// Consolidating tiny coins too — cleaning up dust must not be expensive.
	if m := StorageMass(repeat(10_000, 500), outs(500*10_000-1000), StorageMassC); m != 0 {
		t.Fatalf("consolidating 500 tiny coins has mass %d, expected 0", m)
	}
}

// Exactly the attack that hit Kaspa: 1 → 238 tiny outputs. It does not fit into a block.
func TestStorageMassStopsKaspaStyleAttack(t *testing.T) {
	m := StorageMass([]int64{1 * POWM}, outs(repeat(10_000, 238)...), StorageMassC) // 238 × 0.0001 POWM
	if m <= MaxBlockStorageMass {
		t.Fatalf("an attack like Kaspa's has mass %d, the block cap is %d — it would pass", m, MaxBlockStorageMass)
	}
}

// An output with a zero value has infinite mass (otherwise a division by zero). A data output does
// not count at all.
func TestStorageMassEdgeCases(t *testing.T) {
	if m := StorageMass([]int64{POWM}, outs(0), StorageMassC); m != math.MaxUint64 {
		t.Fatalf("an output with zero has mass %d, expected infinite", m)
	}
	data := []TxOut{{Value: 0, ScriptVersion: ScriptVersionData, Script: []byte("x")}}
	if m := StorageMass([]int64{POWM}, data, StorageMassC); m != 0 {
		t.Fatalf("a data output has mass %d, expected 0", m)
	}
	// The relaxed 2 → 2 formula: the mass is compared harmonically on both sides.
	if m := StorageMass([]int64{3 * POWM, 5 * POWM}, outs(4*POWM, 4*POWM-1000), StorageMassC); m > 5 {
		t.Fatalf("2 → 2 with similar amounts has mass %d", m)
	}
}

// Splitting the attack across several transactions does not help — the cap is on the whole block.
func TestStorageMassSplittingAcrossTxsDoesNotHelp(t *testing.T) {
	// 40 transactions, each 1 → 10 × 0.001 POWM: individually they fit, together they do not.
	one := StorageMass([]int64{POWM}, outs(repeat(100_000, 10)...), StorageMassC)
	if one > MaxBlockStorageMass {
		t.Fatalf("one transaction (mass %d) should fit", one)
	}
	if 40*one <= MaxBlockStorageMass {
		t.Fatalf("40 such transactions (mass %d) would fit — splitting would help", 40*one)
	}
}

// Integration: a block with a coinbase with tiny outputs is invalid — tiny outputs cannot be
// smuggled through the coinbase.
func TestBlockRejectsDustyCoinbase(t *testing.T) {
	p := RegTest
	h := uint64(DevFeeEndHeight + 10) // without the dev fee, so this is only about the outputs
	cb := coinbaseTx(h, Subsidy(h), p)
	pay := cb.Outputs[0].Value
	cb.Outputs = nil
	for range 200 { // 200 × 1 000 cogs
		cb.Outputs = append(cb.Outputs, TxOut{Value: 1000, Script: make([]byte, 32)})
		pay -= 1000
	}
	cb.Outputs = append(cb.Outputs, TxOut{Value: pay, Script: make([]byte, 32)})
	b := blockWith(h, []*Tx{cb})
	if err := b.ValidateBlock(NewUTXOSet(), p); !errors.Is(err, ErrStorageMass) {
		t.Fatalf("a coinbase with dust: %v, expected ErrStorageMass", err)
	}
}

// Integration: a block where every transaction passes on its own but together they exceed the cap is invalid.
func TestBlockRejectsSumOverCap(t *testing.T) {
	p := RegTest
	u := NewUTXOSet()
	sk, pk := testKey(t, 9)
	h := uint64(1000)
	var txs []*Tx
	txs = append(txs, coinbaseTx(h, MinerSubsidy(h), p))
	for i := range 40 {
		op := Outpoint{TxID: [32]byte{byte(i + 1)}, Index: 0}
		coin := Coin{Value: POWM, Script: pk, Height: 5}
		u.AddCoin(op, coin)
		tx := signedSpend(t, sk, op, coin, outs(repeat(100_000, 10)...)) // 10 × 0.001 POWM
		txs = append(txs, tx)
	}
	b := blockWith(h, txs)
	if err := b.ValidateBlock(u, p); !errors.Is(err, ErrStorageMass) {
		t.Fatalf("40 transactions with tiny outputs: %v, expected ErrStorageMass", err)
	}
}

// The mempool refuses a transaction that would not fit into a block even on its own, and asks a
// higher fee for tiny outputs (policy).
func TestMempoolStorageMassPolicy(t *testing.T) {
	m, u, sk, _, op, coin := poolSetup(t)
	// 50 × 0.0001 POWM (the coin holds 0.01 POWM) — it does not fit even into an empty block
	dust := signedSpend(t, sk, op, coin, outs(repeat(10_000, 50)...))
	if _, err := m.Accept(dust, u, 200); !errors.Is(err, ErrTxStorageMass) {
		t.Fatalf("dust into the mempool: %v, expected ErrTxStorageMass", err)
	}
	// A tiny payment of 0.0001 POWM with the minimum fee by bytes: policy wants more.
	small := signedSpend(t, sk, op, coin, outs(10_000, coin.Value-10_000-300))
	if _, err := m.Accept(small, u, 200); !errors.Is(err, ErrFeeTooLow) {
		t.Fatalf("a tiny payment with a fee by bytes: %v, expected ErrFeeTooLow", err)
	}
	// The same payment with a fee by weight passes.
	w := PolicyWeight(len(small.Serialize()), []int64{coin.Value}, small.Outputs)
	fee := int64(w)*MinRelayFeePerKB/1000 + int64(w)*MinRelayFeePerKB/100_000 // +1 %: smaller change raises the weight
	ok := signedSpend(t, sk, op, coin, outs(10_000, coin.Value-10_000-fee))
	if _, err := m.Accept(ok, u, 200); err != nil {
		t.Fatalf("a tiny payment with a fee by weight: %v", err)
	}
}
