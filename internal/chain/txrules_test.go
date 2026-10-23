package chain

import (
	"errors"
	"testing"
)

// noForks is a network with an empty fork table: the rules as they were at the genesis of the
// running testnet.
func noForks(p NetParams) NetParams {
	p.Forks = nil
	return p
}

// A signature made on one network must be worth nothing on another. Before the fork the signed
// message did not say where it was made, so the very same signature verified everywhere — the
// first half of the test shows that, the second that it no longer does.
func TestSignatureIsBoundToItsNetwork(t *testing.T) {
	tx, coins, sk, _ := spendSetup(t)

	oldTest, oldReg := noForks(TestNet).At(1), noForks(RegTest).At(1)
	if SigHash(tx, 0, coins, oldTest) != SigHash(tx, 0, coins, oldReg) {
		t.Fatal("setup: below the fork two networks should sign the same message")
	}
	sig, err := SignInput(tx, 0, coins, sk, oldTest)
	if err != nil {
		t.Fatal(err)
	}
	tx.Witness = []Witness{{Items: [][]byte{sig}}}
	if err := VerifyInput(tx, 0, coins, oldReg); err != nil {
		t.Fatalf("setup: below the fork a signature is expected to carry over: %v", err)
	}

	onTest := TestNet
	onTest.Forks = []ForkAt{{Fork: ForkSignedNetwork, Height: 0}}
	bound, reg := onTest.At(1), RegTest.At(1)
	if SigHash(tx, 0, coins, bound) == SigHash(tx, 0, coins, reg) {
		t.Fatal("two networks sign the same message")
	}
	if SigHash(tx, 0, coins, bound) == SigHash(tx, 0, coins, oldTest) {
		t.Fatal("the bound message is the old one")
	}
	sig, err = SignInput(tx, 0, coins, sk, bound)
	if err != nil {
		t.Fatal(err)
	}
	tx.Witness = []Witness{{Items: [][]byte{sig}}}
	if err := VerifyInput(tx, 0, coins, bound); err != nil {
		t.Fatalf("the signature on its own network: %v", err)
	}
	if err := VerifyInput(tx, 0, coins, reg); !errors.Is(err, ErrBadTxSignature) {
		t.Fatalf("a testnet signature on regtest: %v, expected ErrBadTxSignature", err)
	}
	// And an old signature is not one a block above the fork can carry.
	if err := VerifyInput(tx, 0, coins, oldTest); !errors.Is(err, ErrBadTxSignature) {
		t.Fatalf("a bound signature below the fork: %v", err)
	}
}

// The rule starts at its height: a transaction is signed for the block it goes into.
func TestSignedNetworkStartsAtItsHeight(t *testing.T) {
	tx, coins, _, _ := spendSetup(t)
	p := noForks(RegTest)
	p.Forks = []ForkAt{{Fork: ForkSignedNetwork, Height: 100}}
	if SigHash(tx, 0, coins, p.At(99)) != SigHash(tx, 0, coins, noForks(RegTest).At(99)) {
		t.Fatal("the message changed one block early")
	}
	if SigHash(tx, 0, coins, p.At(100)) != SigHash(tx, 0, coins, RegTest.At(100)) {
		t.Fatal("the message did not change at the fork height")
	}
}

// Every network that has a genesis and the fork in its table really binds: a table entry on a
// network without a root would be a rule that silently does nothing.
func TestNetworksWithAGenesisBindSignatures(t *testing.T) {
	for _, p := range []NetParams{TestNet, RegTest} {
		q := p
		q.Forks = []ForkAt{{Fork: ForkSignedNetwork, Height: 0}}
		id, bound := q.At(1).signedNetwork()
		if !bound || id != Genesis(p).ID() {
			t.Fatalf("%s: bound %v to %x", p.Name, bound, id[:6])
		}
	}
	if Genesis(MainNet) != nil {
		if _, bound := MainNet.At(1).signedNetwork(); !bound {
			t.Fatal("mainnet has a genesis and does not bind signatures to it")
		}
	}
}

// The signed message is part of what every wallet has to reproduce byte for byte, so it is pinned.
func TestSigHashVectors(t *testing.T) {
	tx := &Tx{Version: 1, LockTime: 7,
		Inputs:  []TxIn{{PrevTxID: [32]byte{1, 2, 3}, PrevIndex: 4}},
		Outputs: []TxOut{{Value: 900, Script: make([]byte, 32)}},
	}
	coins := []Coin{{Value: 1000, Script: make([]byte, 32)}}
	for _, c := range []struct {
		name  string
		rules Rules
		want  string
	}{
		{"unbound", noForks(RegTest).At(1), sighashVectorUnbound},
		{"regtest", RegTest.At(1), sighashVectorRegtest},
	} {
		if got := hexOf(SigHash(tx, 0, coins, c.rules)); got != c.want {
			t.Fatalf("%s: sighash %s, pinned %s", c.name, got, c.want)
		}
	}
}

func hexOf(h [32]byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 0, 64)
	for _, b := range h {
		out = append(out, digits[b>>4], digits[b&15])
	}
	return string(out)
}

// The lock time is a height: not below it, from it on yes.
func TestLockTimeIsAHeight(t *testing.T) {
	tx := &Tx{Version: 1, LockTime: 100,
		Inputs:  []TxIn{{PrevTxID: [32]byte{7}}},
		Outputs: []TxOut{{Value: 1, Script: make([]byte, 32)}},
	}
	if err := tx.CheckRules(RegTest.At(99)); !errors.Is(err, ErrTxLocked) {
		t.Fatalf("one block below the lock: %v, expected ErrTxLocked", err)
	}
	for _, h := range []uint64{100, 101, 1 << 40} {
		if err := tx.CheckRules(RegTest.At(h)); err != nil {
			t.Fatalf("height %d, lock 100: %v", h, err)
		}
	}
	tx.LockTime = 0
	if err := tx.CheckRules(RegTest.At(0)); err != nil {
		t.Fatalf("no lock: %v", err)
	}
	// Below the fork the field was ignored, and a chain that has such transactions keeps them.
	tx.LockTime = 1 << 31
	if err := tx.CheckRules(noForks(RegTest).At(5)); err != nil {
		t.Fatalf("below the fork: %v", err)
	}
}

// The sequence is reserved: any value but zero is refused, so a later rule can give the other
// values a meaning without old transactions having claimed them.
func TestSequenceIsReserved(t *testing.T) {
	tx := &Tx{Version: 1,
		Inputs:  []TxIn{{PrevTxID: [32]byte{7}}, {PrevTxID: [32]byte{8}, Sequence: 1}},
		Outputs: []TxOut{{Value: 1, Script: make([]byte, 32)}},
	}
	if err := tx.CheckRules(RegTest.At(5)); !errors.Is(err, ErrTxBadSequence) {
		t.Fatalf("a non-zero sequence: %v, expected ErrTxBadSequence", err)
	}
	if err := tx.CheckRules(noForks(RegTest).At(5)); err != nil {
		t.Fatalf("below the fork: %v", err)
	}
	tx.Inputs[1].Sequence = 0
	if err := tx.CheckRules(RegTest.At(5)); err != nil {
		t.Fatal(err)
	}
}

// A locked transaction is not kept in the mempool waiting — it is refused, and accepted once the
// next block is one it may go into. In a block below its height it makes the block invalid.
func TestLockedTransactionWaitsOutsideTheMempool(t *testing.T) {
	m, u, sk, pk, op, coin := poolSetup(t)
	tx := &Tx{Version: 1, LockTime: 50,
		Inputs:  []TxIn{{PrevTxID: op.TxID, PrevIndex: op.Index}},
		Outputs: []TxOut{{Value: 900_000, Script: pk}}}
	sig, err := SignInput(tx, 0, []Coin{coin}, sk, RegTest.At(50))
	if err != nil {
		t.Fatal(err)
	}
	tx.Witness = []Witness{{Items: [][]byte{sig}}}

	if _, err := m.Accept(tx, u, 49); !errors.Is(err, ErrTxLocked) {
		t.Fatalf("the next block is 49, the lock 50: %v", err)
	}
	if m.Len() != 0 {
		t.Fatal("a locked transaction is in the mempool")
	}
	early := blockWith(49, []*Tx{coinbaseTx(49, 0, RegTest), tx})
	if err := early.ValidateBlock(u, RegTest); !errors.Is(err, ErrTxLocked) {
		t.Fatalf("a block below the lock: %v, expected ErrTxLocked", err)
	}
	if _, err := m.Accept(tx, u, 50); err != nil {
		t.Fatalf("the next block is 50, the lock 50: %v", err)
	}
	onTime := blockWith(50, []*Tx{coinbaseTx(50, 0, RegTest), tx})
	if err := onTime.ValidateBlock(u, RegTest); err != nil {
		t.Fatalf("a block at the lock height: %v", err)
	}
}

// An output is a 32-byte key or a data output; anything else cannot be created.
func TestOutputsHaveToBeSpendable(t *testing.T) {
	mk := func(out TxOut) *Tx {
		return &Tx{Version: 1, Inputs: []TxIn{{PrevTxID: [32]byte{7}}}, Outputs: []TxOut{out}}
	}
	at, before := RegTest.At(5), noForks(RegTest).At(5)

	for _, n := range []int{1, 20, 31, 33, 64} {
		tx := mk(TxOut{Value: 1, Script: make([]byte, n)})
		if err := tx.CheckRules(at); !errors.Is(err, ErrBadPubkeyScript) {
			t.Fatalf("a version 0 output of %d B: %v, expected ErrBadPubkeyScript", n, err)
		}
		if err := tx.CheckRules(before); err != nil {
			t.Fatalf("below the fork, %d B: %v", n, err)
		}
	}
	for v := uint8(1); v <= ScriptVersionMaxReserved; v++ {
		tx := mk(TxOut{Value: 1, ScriptVersion: v, Script: make([]byte, 32)})
		if err := tx.CheckRules(at); !errors.Is(err, ErrTxBadVersion) {
			t.Fatalf("an output of the reserved version %d: %v, expected ErrTxBadVersion", v, err)
		}
		if err := tx.CheckRules(before); err != nil {
			t.Fatalf("below the fork, version %d: %v", v, err)
		}
	}
	for _, ok := range []TxOut{
		{Value: 1, Script: make([]byte, 32)},
		{ScriptVersion: ScriptVersionData, Script: []byte("an anchor")},
		{ScriptVersion: ScriptVersionData},
	} {
		if err := mk(ok).CheckRules(at); err != nil {
			t.Fatalf("output %+v: %v", ok, err)
		}
	}
}

// The dev fee output is an output like any other, so from the fork on it is a key. The placeholder
// the running testnet pays to is not one, and the change of the script has to land on exactly
// the fork height — one block off in either direction and every coinbase is refused.
func TestDevFeeIsPaidToAKeyFromTheFork(t *testing.T) {
	for _, p := range []NetParams{TestNet, RegTest} {
		if len(p.DevFeeKey) != PubkeyBytes {
			t.Fatalf("%s: the dev fee key has %d B", p.Name, len(p.DevFeeKey))
		}
		if h, ok := p.ForkHeight(ForkStrictOutputs); ok {
			if got := p.DevFeeScriptAt(h); len(got) != PubkeyBytes {
				t.Fatalf("%s: at the fork the dev fee goes to %d B", p.Name, len(got))
			}
		}
	}
	if string(TestNet.DevFeeKey) == string(RegTest.DevFeeKey) {
		t.Fatal("two networks share a dev fee key")
	}

	const fork = 50
	p := noForks(RegTest)
	p.Forks = []ForkAt{{Fork: ForkStrictOutputs, Height: fork}}
	for _, h := range []uint64{fork - 1, fork} {
		cb := BuildCoinbase(h, nil, [32]byte{}, p)
		b := &Block{Header: &Header{Height: uint32(h)}, Txs: []*Tx{cb}}
		SealCoinbase(b.Txs, CreditsRoot(0, nil))
		b.Seal()
		if err := b.ValidateBlock(NewUTXOSet(), p); err != nil {
			t.Fatalf("our own coinbase at height %d (fork at %d): %v", h, fork, err)
		}
		want := p.DevFeeScript
		if h >= fork {
			want = p.DevFeeKey
		}
		if string(cb.Outputs[0].Script) != string(want) {
			t.Fatalf("height %d: the dev fee goes to %q", h, cb.Outputs[0].Script)
		}
	}
	// The old placeholder at the fork height is refused.
	stale := BuildCoinbase(fork, nil, [32]byte{}, noForks(RegTest))
	b := &Block{Header: &Header{Height: fork}, Txs: []*Tx{stale}}
	SealCoinbase(b.Txs, CreditsRoot(0, nil))
	b.Seal()
	if err := b.ValidateBlock(NewUTXOSet(), p); err == nil {
		t.Fatal("a coinbase paying the old placeholder validated at the fork height")
	}
}

const (
	sighashVectorUnbound = "bef5e13a04b33dfbb07a9593d4a3c2c8f5e6679457dcbf7d43202715bd1a22af"
	sighashVectorRegtest = "8f4fe65c9a2e2ed1191beb11e761a4f21aca7a1b89e911c79e4187398385d0b9"
)

// What sits in the mempool was valid for the block before the fork. Checked again for the fork
// height, a transaction signed the old way falls out — it could never be mined again.
func TestMempoolIsCheckedAgainAtAFork(t *testing.T) {
	const fork = 10
	p := noForks(RegTest)
	p.Forks = []ForkAt{{Fork: ForkSignedNetwork, Height: fork}}
	if !p.RulesChangeAt(fork) || p.RulesChangeAt(fork-1) || p.RulesChangeAt(fork+1) {
		t.Fatal("RulesChangeAt does not point at the fork height and only there")
	}

	_, u, sk, pk, op, coin := poolSetup(t)
	m := NewMempool(p)
	tx := &Tx{Version: 1, Inputs: []TxIn{{PrevTxID: op.TxID, PrevIndex: op.Index}},
		Outputs: []TxOut{{Value: 900_000, Script: pk}}}
	sig, err := SignInput(tx, 0, []Coin{coin}, sk, p.At(fork-1))
	if err != nil {
		t.Fatal(err)
	}
	tx.Witness = []Witness{{Items: [][]byte{sig}}}
	if _, err := m.Accept(tx, u, fork-1); err != nil {
		t.Fatalf("below the fork: %v", err)
	}
	if dropped := m.Revalidate(u, fork-1); dropped != 0 || m.Len() != 1 {
		t.Fatalf("checked again for the same height: %d dropped", dropped)
	}
	if dropped := m.Revalidate(u, fork); dropped != 1 || m.Len() != 0 {
		t.Fatalf("checked for the fork height: %d dropped, %d left", dropped, m.Len())
	}
	// Signed for the fork height it is accepted there.
	sig, err = SignInput(tx, 0, []Coin{coin}, sk, p.At(fork))
	if err != nil {
		t.Fatal(err)
	}
	tx.Witness = []Witness{{Items: [][]byte{sig}}}
	if _, err := m.Accept(tx, u, fork); err != nil {
		t.Fatalf("signed for the fork height: %v", err)
	}
}
