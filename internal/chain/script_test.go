package chain

import (
	"errors"
	"testing"
)

// spendSetup prepares an output with a key and a transaction that spends it.
func spendSetup(t *testing.T) (*Tx, []Coin, *[32]byte, []byte) {
	t.Helper()
	sk, pk := testKey(t, 21)
	coin := Coin{Value: 1000, Script: pk, Height: 1}
	tx := &Tx{Version: 1,
		Inputs:  []TxIn{{PrevTxID: [32]byte{7}, PrevIndex: 0}},
		Outputs: []TxOut{{Value: 900, Script: pk}},
	}
	coins := []Coin{coin}
	sig, err := SignInput(tx, 0, coins, sk, RegTest.At(1))
	if err != nil {
		t.Fatal(err)
	}
	tx.Witness = []Witness{{Items: [][]byte{sig}}}
	return tx, coins, sk, pk
}

func TestValidSignatureAccepted(t *testing.T) {
	tx, coins, _, _ := spendSetup(t)
	if err := VerifyInput(tx, 0, coins, RegTest.At(1)); err != nil {
		t.Fatalf("a valid signature was refused: %v", err)
	}
}

// The signature commits to the outputs — after they change it is no longer valid. Without that an
// attacker would redirect somebody else's transaction to their own address.
func TestSignatureCommitsToOutputs(t *testing.T) {
	tx, coins, _, _ := spendSetup(t)

	_, attacker := testKey(t, 99)
	tx.Outputs[0].Script = attacker
	if err := VerifyInput(tx, 0, coins, RegTest.At(1)); !errors.Is(err, ErrBadTxSignature) {
		t.Fatalf("after redirecting the output the signature has to fail, got %v", err)
	}
}

func TestSignatureCommitsToOutputValue(t *testing.T) {
	tx, coins, _, _ := spendSetup(t)
	tx.Outputs[0].Value = 950
	if err := VerifyInput(tx, 0, coins, RegTest.At(1)); !errors.Is(err, ErrBadTxSignature) {
		t.Fatalf("after changing the amount the signature has to fail, got %v", err)
	}
}

// The sighash also commits to the amount of the output being spent — that is the lesson from
// BIP143. Without it a different input value can be slipped to the signer and a fee lured out of them.
func TestSignatureCommitsToInputAmount(t *testing.T) {
	tx, coins, _, _ := spendSetup(t)
	coins[0].Value = 100_000
	if err := VerifyInput(tx, 0, coins, RegTest.At(1)); !errors.Is(err, ErrBadTxSignature) {
		t.Fatalf("after changing the input amount the signature has to fail, got %v", err)
	}
}

func TestSignatureCommitsToPrevout(t *testing.T) {
	tx, coins, _, _ := spendSetup(t)
	tx.Inputs[0].PrevIndex = 1
	if err := VerifyInput(tx, 0, coins, RegTest.At(1)); !errors.Is(err, ErrBadTxSignature) {
		t.Fatalf("after changing the outpoint the signature has to fail, got %v", err)
	}
}

func TestSignatureFromWrongKeyRejected(t *testing.T) {
	tx, coins, _, _ := spendSetup(t)
	otherSk, _ := testKey(t, 77)
	sig, err := SignInput(tx, 0, coins, otherSk, RegTest.At(1))
	if err != nil {
		t.Fatal(err)
	}
	tx.Witness[0].Items[0] = sig
	if err := VerifyInput(tx, 0, coins, RegTest.At(1)); !errors.Is(err, ErrBadTxSignature) {
		t.Fatalf("a signature with the wrong key has to fail, got %v", err)
	}
}

func TestMalformedWitnessRejected(t *testing.T) {
	tx, coins, _, _ := spendSetup(t)

	orig := tx.Witness
	tx.Witness = nil
	if err := VerifyInput(tx, 0, coins, RegTest.At(1)); !errors.Is(err, ErrMissingWitness) {
		t.Fatalf("a missing witness should give ErrMissingWitness, got %v", err)
	}
	tx.Witness = []Witness{{Items: [][]byte{orig[0].Items[0], orig[0].Items[0]}}}
	if err := VerifyInput(tx, 0, coins, RegTest.At(1)); !errors.Is(err, ErrMissingWitness) {
		t.Fatalf("two items in the witness have to be refused, got %v", err)
	}
	tx.Witness = []Witness{{Items: [][]byte{make([]byte, 63)}}}
	if err := VerifyInput(tx, 0, coins, RegTest.At(1)); !errors.Is(err, ErrBadTxSignature) {
		t.Fatalf("a short signature has to be refused, got %v", err)
	}
}

func TestBadPubkeyScriptRejected(t *testing.T) {
	tx, coins, _, _ := spendSetup(t)
	coins[0].Script = []byte("this is not a key")
	if err := VerifyInput(tx, 0, coins, RegTest.At(1)); !errors.Is(err, ErrBadPubkeyScript) {
		t.Fatalf("an output without a 32-byte key has to be refused, got %v", err)
	}
}

// An output of a reserved version has no spending rule, so it cannot be spent — by nobody, until
// a fork defines the version. Before ForkStrictOutputs anybody could spend one.
func TestReservedVersionsCannotBeSpent(t *testing.T) {
	tx, coins, _, _ := spendSetup(t)
	tx.Witness = nil
	before := RegTest
	before.Forks = nil
	for v := uint8(1); v <= ScriptVersionMaxReserved; v++ {
		coins[0].ScriptVersion = v
		if err := VerifyInput(tx, 0, coins, RegTest.At(1)); !errors.Is(err, ErrTxBadVersion) {
			t.Fatalf("the reserved version %d was spent: %v", v, err)
		}
		if err := VerifyInput(tx, 0, coins, before.At(1)); err != nil {
			t.Fatalf("below the fork the reserved version %d was spendable by anybody: %v", v, err)
		}
	}
}

// Two different inputs of the same transaction have different sighashes — otherwise a signature could be moved.
func TestSigHashDiffersPerInput(t *testing.T) {
	_, pk := testKey(t, 31)
	tx := &Tx{Version: 1,
		Inputs:  []TxIn{{PrevTxID: [32]byte{1}, PrevIndex: 0}, {PrevTxID: [32]byte{2}, PrevIndex: 0}},
		Outputs: []TxOut{{Value: 10, Script: pk}},
	}
	coins := []Coin{{Value: 5, Script: pk}, {Value: 10, Script: pk}}
	if SigHash(tx, 0, coins, RegTest.At(1)) == SigHash(tx, 1, coins, RegTest.At(1)) {
		t.Fatal("the sighash has to differ by input index")
	}
}
