package chain

import (
	"errors"
	"testing"
)

// sealedSpend returns a UTXO set with one coin and a block at the given height that spends it
// with a valid signature.
func sealedSpend(t *testing.T, height uint64, params NetParams) (*UTXOSet, *Block) {
	t.Helper()
	sk, pk := testKey(t, 7)
	u := NewUTXOSet()
	prev := Outpoint{TxID: [32]byte{9}, Index: 0}
	coin := Coin{Value: 10 * POWM, Script: pk, Height: 1}
	u.AddCoin(prev, coin)

	spend := &Tx{Version: 1,
		Inputs:  []TxIn{{PrevTxID: prev.TxID, PrevIndex: 0}},
		Outputs: []TxOut{{Value: 9 * POWM, Script: pk}},
	}
	sig, err := SignInput(spend, 0, []Coin{coin}, sk)
	if err != nil {
		t.Fatal(err)
	}
	spend.Witness = []Witness{{Items: [][]byte{sig}}}

	txs := []*Tx{coinbaseTx(height, MinerSubsidy(height), params), spend}
	b := &Block{Header: &Header{Version: 1, Height: uint32(height), Bits: RegtestTargetBits,
		Time: 1_700_000_500}, Txs: txs}
	SealCoinbase(txs, height, params)
	b.Header.MerkleRoot = TxMerkleRoot(txs)
	return u, b
}

// copyOf returns a deep copy of a block, the way a peer holds it.
func copyOf(t *testing.T, b *Block) *Block {
	t.Helper()
	c, err := DeserializeBlock(b.Serialize())
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// The point of the rule: a changed witness leaves the block id and the merkle root alone, and
// the commitment still catches it — with no UTXO set at hand.
func TestWitnessCommitmentCatchesAChangedWitness(t *testing.T) {
	p := RegTest
	u, b := sealedSpend(t, 10, p)
	if err := b.ValidateBlock(u, p); err != nil {
		t.Fatalf("the block as mined: %v", err)
	}
	if err := b.CheckWitnesses(p); err != nil || !b.WitnessesCommitted(p) {
		t.Fatalf("the block as mined does not match its own commitment: %v", err)
	}

	changed := copyOf(t, b)
	changed.Txs[1].Witness[0].Items[0][5] ^= 1
	if changed.Header.ID() != b.Header.ID() || TxMerkleRoot(changed.Txs) != b.Header.MerkleRoot {
		t.Fatal("setup: the changed copy no longer matches the header")
	}
	if err := changed.CheckWitnesses(p); !errors.Is(err, ErrBadWitnessCommitment) {
		t.Fatalf("a changed signature: %v, expected ErrBadWitnessCommitment", err)
	}
	if changed.WitnessesCommitted(p) {
		t.Fatal("a changed copy counts as committed")
	}
	if err := changed.ValidateBlock(u, p); err == nil {
		t.Fatal("a changed copy validated")
	}
	// The committed part is untouched, so the block itself is not to blame.
	if err := changed.ValidateCommitted(u, p); err != nil {
		t.Fatalf("the committed part of a changed copy: %v", err)
	}

	// A witness taken away altogether, and one item more — neither is the miner's block.
	stripped := copyOf(t, b)
	stripped.Txs[1].Witness = nil
	if err := stripped.CheckWitnesses(p); !errors.Is(err, ErrBadWitnessCommitment) {
		t.Fatalf("a stripped witness: %v", err)
	}
	padded := copyOf(t, b)
	padded.Txs[1].Witness[0].Items = append(padded.Txs[1].Witness[0].Items, []byte{1})
	if err := padded.CheckWitnesses(p); !errors.Is(err, ErrBadWitnessCommitment) {
		t.Fatalf("a padded witness: %v", err)
	}
}

// The coinbase's own leaf is not under the commitment, so a witness on a coinbase would be free
// bytes for anybody passing the block on. It is refused as a fault of the copy.
func TestCoinbaseCarriesNoWitness(t *testing.T) {
	p := RegTest
	u, b := sealedSpend(t, 10, p)
	padded := copyOf(t, b)
	padded.Txs[0].Witness = []Witness{{Items: [][]byte{{1, 2, 3}}}}
	if padded.Header.ID() != b.Header.ID() || TxMerkleRoot(padded.Txs) != b.Header.MerkleRoot {
		t.Fatal("setup: the padded copy no longer matches the header")
	}
	if err := padded.CheckWitnesses(p); !errors.Is(err, ErrBadWitnessCommitment) {
		t.Fatalf("a witness on the coinbase: %v, expected ErrBadWitnessCommitment", err)
	}
	if err := padded.ValidateBlock(u, p); !errors.Is(err, ErrBadWitnessCommitment) {
		t.Fatalf("validation of a witness on the coinbase: %v", err)
	}
}

// A coinbase without the commitment — or with it anywhere but last, or of the wrong shape — makes
// the block invalid, and that is the block's fault: the outputs are part of the txid.
func TestBlockWithoutACommitmentIsInvalid(t *testing.T) {
	p := RegTest
	const h = 10
	build := func(mutate func(cb *Tx)) (*UTXOSet, *Block) {
		u, b := sealedSpend(t, h, p)
		mutate(b.Txs[0])
		b.Header.MerkleRoot = TxMerkleRoot(b.Txs)
		return u, b
	}
	cases := map[string]func(cb *Tx){
		"no commitment": func(cb *Tx) { cb.Outputs = cb.Outputs[:len(cb.Outputs)-1] },
		"not the last output": func(cb *Tx) {
			n := len(cb.Outputs)
			cb.Outputs[n-1], cb.Outputs[n-2] = cb.Outputs[n-2], cb.Outputs[n-1]
		},
		"a wrong tag": func(cb *Tx) { cb.Outputs[len(cb.Outputs)-1].Script[0] ^= 1 },
		"too short":   func(cb *Tx) { o := &cb.Outputs[len(cb.Outputs)-1]; o.Script = o.Script[:len(o.Script)-1] },
		"too long":    func(cb *Tx) { o := &cb.Outputs[len(cb.Outputs)-1]; o.Script = append(o.Script, 0) },
	}
	for name, mutate := range cases {
		u, b := build(mutate)
		if err := b.ValidateBlock(u, p); !errors.Is(err, ErrNoWitnessCommitment) {
			t.Fatalf("%s: %v, expected ErrNoWitnessCommitment", name, err)
		}
		// With the witnesses out of the picture it is still invalid: no copy can fix it.
		if err := b.ValidateCommitted(u, p); !errors.Is(err, ErrNoWitnessCommitment) {
			t.Fatalf("%s: the committed part passed (%v)", name, err)
		}
		if err := b.CheckWitnesses(p); !errors.Is(err, ErrNoWitnessCommitment) {
			t.Fatalf("%s: CheckWitnesses says %v", name, err)
		}
	}

	// The right shape and a wrong root: this body is not the one the miner committed to — or the
	// miner committed to nothing that exists. Either way no witness set seen so far matches.
	u, b := build(func(cb *Tx) { cb.Outputs[len(cb.Outputs)-1].Script[20] ^= 1 })
	if err := b.ValidateBlock(u, p); !errors.Is(err, ErrBadWitnessCommitment) {
		t.Fatalf("a wrong root: %v, expected ErrBadWitnessCommitment", err)
	}
}

// The rule starts at its height and not a block earlier: below it a block needs no commitment
// and SealCoinbase adds none, so the blocks of an old chain stay valid and stay what they were.
func TestWitnessCommitmentStartsAtItsHeight(t *testing.T) {
	const fork = 50
	p := RegTest
	p.Forks = []ForkAt{{Fork: ForkWitnessCommitment, Height: fork}}

	u, before := sealedSpend(t, fork-1, p)
	if _, err := before.witnessCommitment(); err == nil {
		t.Fatal("SealCoinbase wrote a commitment below the fork")
	}
	if err := before.ValidateBlock(u, p); err != nil {
		t.Fatalf("a block below the fork without a commitment: %v", err)
	}
	if err := before.CheckWitnesses(p); err != nil || before.WitnessesCommitted(p) {
		t.Fatalf("below the fork there is nothing to check and nothing committed (%v)", err)
	}
	// Below the fork a changed witness cannot be told on arrival.
	changed := copyOf(t, before)
	changed.Txs[1].Witness[0].Items[0][5] ^= 1
	if err := changed.CheckWitnesses(p); err != nil {
		t.Fatalf("below the fork: %v", err)
	}

	u, at := sealedSpend(t, fork, p)
	if _, err := at.witnessCommitment(); err != nil {
		t.Fatal("SealCoinbase wrote no commitment at the fork height")
	}
	if err := at.ValidateBlock(u, p); err != nil {
		t.Fatalf("a sealed block at the fork height: %v", err)
	}
	// The same block built the old way is refused from this height on.
	never := p
	never.Forks = nil
	u, old := sealedSpend(t, fork, never)
	if err := old.ValidateBlock(u, p); !errors.Is(err, ErrNoWitnessCommitment) {
		t.Fatalf("an unsealed block at the fork height: %v, expected ErrNoWitnessCommitment", err)
	}
}

// Sealing takes over a data output that is already last instead of adding a second one — a
// transaction may carry one — and sealing twice changes nothing.
func TestSealCoinbaseKeepsOneDataOutput(t *testing.T) {
	p := RegTest
	const h = 10
	cb := coinbaseTx(h, MinerSubsidy(h), p)
	cb.Outputs = append(cb.Outputs, TxOut{ScriptVersion: ScriptVersionData, Script: []byte("x")})
	txs := []*Tx{cb}
	n := len(cb.Outputs)

	SealCoinbase(txs, h, p)
	first := cb.TxID()
	SealCoinbase(txs, h, p)
	if len(cb.Outputs) != n || cb.TxID() != first {
		t.Fatalf("%d outputs after sealing twice, expected %d and the same txid", len(cb.Outputs), n)
	}
	if err := cb.CheckSanity(); err != nil {
		t.Fatalf("the sealed coinbase: %v", err)
	}
	b := &Block{Header: &Header{Height: h}, Txs: txs}
	if err := b.CheckWitnesses(p); err != nil {
		t.Fatal(err)
	}
}
