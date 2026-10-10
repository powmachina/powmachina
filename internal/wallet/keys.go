package wallet

import (
	"fmt"

	"powmachina/internal/hd"
	"powmachina/internal/mh"
)

// The wallet's keys.
//
// A BIP32 tree is derived from the 24 words (BIP39), with the account path `m/44'/9797'/0'` (SLIP-44
// coin_type 9797). Under it there are two branches, as in BIP44:
//
//	…/0/i  the receiving addresses (the ones you give to people and an exchange gives to its users)
//	…/1/i  change (the remainder from your own payments — never shown to anybody)
//
// On top of that there are imported keys (`importprivkey`), which are **not covered by the word backup**.

// AccountPath is the account's path (SPEC §4).
const AccountPath = "m/44'/9797'/0'"

const (
	branchReceive = 0
	branchChange  = 1
	kindImported  = 2
)

// How many addresses ahead the wallet watches. On a restore from the words it finds the coins even on
// addresses that were handed out but have received nothing yet — as long as the gap is smaller than the
// lookahead. An exchange hands out addresses in bulk and not every one gets a deposit, which is why the
// receiving lookahead is generous.
const (
	receiveLookahead = 1000
	changeLookahead  = 100
)

// keyRef says where a key comes from.
type keyRef struct {
	kind  uint8 // branchReceive, branchChange, kindImported
	index uint32
}

type keychain struct {
	receive, change *hd.Key
	// derived: public key → origin; secrets: a cache of private keys (both derived and imported)
	derived map[[32]byte]keyRef
	secrets map[[32]byte]*[32]byte
	// how many keys of each branch are derived (including the lookahead)
	derivedRecv, derivedChange uint32
}

func newKeychain(seed []byte) (*keychain, error) {
	master, err := hd.NewMaster(seed)
	if err != nil {
		return nil, err
	}
	account, err := master.Derive(AccountPath)
	if err != nil {
		return nil, err
	}
	recv, err := account.Child(branchReceive)
	if err != nil {
		return nil, err
	}
	change, err := account.Child(branchChange)
	if err != nil {
		return nil, err
	}
	return &keychain{receive: recv, change: change,
		derived: make(map[[32]byte]keyRef), secrets: make(map[[32]byte]*[32]byte)}, nil
}

// ensure derives a branch's keys up to the index `upto` (exclusive).
func (k *keychain) ensure(branch uint8, upto uint32) error {
	parent, have := k.receive, &k.derivedRecv
	if branch == branchChange {
		parent, have = k.change, &k.derivedChange
	}
	for ; *have < upto; *have++ {
		child, err := parent.Child(*have)
		if err != nil {
			// BIP32: an invalid child (probability ~2⁻¹²⁷) is skipped. The index stays
			// "used", we never hand out an address from it.
			continue
		}
		pub, err := child.XOnlyPubkey()
		if err != nil {
			continue
		}
		k.derived[pub] = keyRef{kind: branch, index: *have}
		k.secrets[pub] = child.PrivateKey()
	}
	return nil
}

// pubAt returns a public key from a branch (it has to be derived already, through ensure).
func (k *keychain) pubAt(branch uint8, index uint32) ([32]byte, error) {
	parent := k.receive
	if branch == branchChange {
		parent = k.change
	}
	child, err := parent.Child(index)
	if err != nil {
		return [32]byte{}, err
	}
	return child.XOnlyPubkey()
}

func (k *keychain) addImported(sk *[32]byte) ([32]byte, error) {
	pub, err := mh.SchnorrPubkey(sk)
	if err != nil {
		return [32]byte{}, fmt.Errorf("wallet: invalid private key: %w", err)
	}
	cp := *sk
	k.secrets[*pub] = &cp
	if _, ok := k.derived[*pub]; !ok {
		k.derived[*pub] = keyRef{kind: kindImported}
	}
	return *pub, nil
}
