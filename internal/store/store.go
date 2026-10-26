// Package store is the node's persistent storage: the blocks, the height index and the UTXO set.
//
// # Why Pebble and not our own code
//
// The project otherwise keeps zero external dependencies (a project rule). This is an exception and a
// deliberate one: a bug in storage means a **silent loss of money**, not a crash somebody notices.
// Pebble is an LSM tree running under CockroachDB in production — atomic batches and the WAL are
// solved and debugged there. Our own implementation would be less code to read, but more places
// where durability can be broken.
//
// # How consistency after a crash is solved
//
// This is the core of the whole design, so in order.
//
// Writing a block and applying it are **two separate operations**:
//
//  1. `PutBlock` stores the header and the body. If the node crashes here, nothing happened — the
//     block is on disk but the UTXO set does not know about it.
//  2. `ApplyBlock` does the following in **one atomic batch**: spends the inputs, creates the
//     outputs, writes the undo data, fills in the height index and **moves the `tip` mark**.
//
// Pebble either writes a batch whole or not at all. That is why **the UTXO set and the tip mark
// cannot diverge** — and that is exactly the state that would force a resync after `kill -9`.
//
// The only difference that can arise after a crash is "the block is stored but not applied".
// That is settled at startup by replaying — the blocks are on disk, so nothing is pulled from the network.
package store

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/cockroachdb/pebble/v2"
	"github.com/cockroachdb/pebble/v2/vfs"
)

// The schema version. On an incompatible change it is raised and the node refuses to open an old
// directory rather than silently reading the wrong data.
const schemaVersion uint32 = 2 // 2: the credit state (prefixCredit, keyPot)

// The prefixes of the key spaces. They are single bytes, so iteration within a space is contiguous
// and cheap.
const (
	prefixHeader  byte = 0x01 // <id:32>            -> the header (89 B)
	prefixBody    byte = 0x02 // <id:32>            -> the serialized block
	prefixHeight  byte = 0x03 // <height:8 BE>      -> the block id on the active branch
	prefixCoin    byte = 0x04 // <txid:32><vout:4>  -> a UTXO entry
	prefixUndo    byte = 0x05 // <id:32>            -> the block's undo data
	prefixTxLoc   byte = 0x06 // <txid:32>          -> <block id:32><position:4>  (optional)
	prefixAddr    byte = 0x07 // <script hash:32><height:8 BE><txid:32><direction:1><n:4> -> the value
	prefixAddrSum byte = 0x08 // <script hash:32>  -> the address summary (optional)
	prefixRich    byte = 0x09 // <inverted balance:8 BE><script hash:32> -> ∅ (the rich list)
	prefixInvalid byte = 0x0A // <id:32>            -> ∅ (a block that failed validation)
	prefixCredit  byte = 0x0B // <payout:32>        -> chain.Credit (the Sharepool's credit state)
	// The credit state's indexes (credits.go): the stored nodes of its tree, its payout queue and
	// the keys by the height they go inactive, the dust apart.
	prefixCreditNode   byte = 0x0C // <depth:1><prefix:32>        -> the subtree's hash
	prefixCreditQueue  byte = 0x0D // <from:8 BE><payout:32>      -> ∅
	prefixCreditExpiry byte = 0x0E // <expiry:8 BE><payout:32>    -> ∅ (the credit is not dust)
	prefixCreditDust   byte = 0x0F // <expiry:8 BE><payout:32>    -> ∅ (the credit is dust)
	// For RPC, not for blocks: the keys by their last share, the most recent first.
	prefixCreditRecent byte = 0x10 // <^last share:8 BE><payout:32> -> ∅
	prefixMeta         byte = 0xFF // the meta keys below
)

var (
	keyVersion = []byte{prefixMeta, 'v', 'e', 'r'}
	// keyTip is the block the UTXO set is applied up to.
	keyTip = []byte{prefixMeta, 't', 'i', 'p'}
	// keyHeaderTip is the height we have verified headers up to.
	//
	// They are two **separate** marks and they have to be. Headers are verified and indexed before
	// the bodies arrive; the bodies are downloaded in parallel and out of order; applying is serial
	// and last. If the height index only came into existence on apply, `ApplyStored` would have
	// nothing to tell which block is next from — the bodies would download and the UTXO set
	// would stay empty.
	keyHeaderTip = []byte{prefixMeta, 'h', 't', 'i', 'p'}
	// keyTxIndexed is the mark that the transaction index covers the whole chain. Without it a
	// reindex is started at startup — an index covering only part of the history would return "not
	// found" for older transactions, and that looks like a valid answer.
	keyTxIndexed = []byte{prefixMeta, 't', 'x', 'i', 'd', 'x'}
	// keyCreditIndexed is the mark that the credit state's indexes cover it, with the version of
	// the indexes (creditIndexVersion). A directory from before them, or from an older version,
	// builds them once when it opens (ensureCreditIndex).
	keyCreditIndexed = []byte{prefixMeta, 'c', 'i', 'd', 'x'}
	// keyCreditCount is how many keys hold a credit (8 B LE), kept with the indexes.
	keyCreditCount = []byte{prefixMeta, 'c', 'c', 'n', 't'}
	// keyAddrIndexed is the same mark for the address index.
	keyAddrIndexed = []byte{prefixMeta, 'a', 'd', 'i', 'd', 'x'}
)

var (
	ErrNotFound    = errors.New("store: the record does not exist")
	ErrWrongSchema = errors.New("store: the directory has a different schema version")
	ErrTipMismatch = errors.New("store: the block does not build on the tip")
	// ErrNoTxIndex means the storage does not keep a transaction index. It is deliberately not
	// ErrNotFound — the caller has to be able to tell a missing transaction from a missing index.
	ErrNoTxIndex = errors.New("store: the transaction index is not enabled (-txindex)")
	// ErrNoAddrIndex means the storage does not keep an address index.
	ErrNoAddrIndex = errors.New("store: the address index is not enabled (-addrindex)")
)

// Store is the persistent storage. It is safe for concurrent use — Pebble handles that itself.
type Store struct {
	db *pebble.DB
	// indexTx turns on the transaction index (txid -> block). See txindex.go.
	indexTx bool
	// indexAddr turns on the address index and the summaries. See addrindex.go.
	indexAddr bool
}

// Options are the choices made when opening the storage.
type Options struct {
	// TxIndex turns on the transaction index. It costs extra space, but without it a transaction
	// cannot be found by txid — the explorer and every exchange need it.
	TxIndex bool
	// AddrIndex turns on the address index and the balance summaries. Without it the explorer cannot
	// show an address's history — Bitcoin Core has no such index at all.
	AddrIndex bool
}

// Open opens or creates the storage in the directory `dir` without the optional indexes.
func Open(dir string) (*Store, error) { return OpenWith(dir, Options{}) }

// OpenWith opens the storage with the given options.
func OpenWith(dir string, opt Options) (*Store, error) {
	db, err := pebble.Open(dir, &pebble.Options{})
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	s := &Store{db: db, indexTx: opt.TxIndex, indexAddr: opt.AddrIndex}
	if err := s.checkSchema(); err != nil {
		db.Close()
		return nil, err
	}
	if err := s.ensureCreditIndex(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) checkSchema() error {
	v, closer, err := s.db.Get(keyVersion)
	switch {
	case errors.Is(err, pebble.ErrNotFound):
		// A fresh directory — write the version with Sync, so it cannot be opened half-empty.
		var buf [4]byte
		binary.BigEndian.PutUint32(buf[:], schemaVersion)
		return s.db.Set(keyVersion, buf[:], pebble.Sync)
	case err != nil:
		return fmt.Errorf("store: %w", err)
	}
	defer closer.Close()
	if got := binary.BigEndian.Uint32(v); got != schemaVersion {
		return fmt.Errorf("%w: got %d, expected %d", ErrWrongSchema, got, schemaVersion)
	}
	return nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

// Flush pushes the memtable to disk. Not needed in ordinary operation — every write that matters
// goes with `pebble.Sync`. It is here for tests and for a clean shutdown.
func (s *Store) Flush() error {
	return s.db.Flush()
}

// ---------------------------------------------------------------- the keys

func keyWithID(prefix byte, id [32]byte) []byte {
	k := make([]byte, 1+32)
	k[0] = prefix
	copy(k[1:], id[:])
	return k
}

func keyHeight(h uint64) []byte {
	k := make([]byte, 1+8)
	k[0] = prefixHeight
	binary.BigEndian.PutUint64(k[1:], h) // BE, so iteration goes by height
	return k
}

func keyCoin(txid [32]byte, index uint32) []byte {
	k := make([]byte, 1+32+4)
	k[0] = prefixCoin
	copy(k[1:], txid[:])
	binary.BigEndian.PutUint32(k[33:], index)
	return k
}

// get is a helper that copies the value and closes the closer. Pebble returns memory that stops
// being valid after `closer.Close()` — forgetting that is the classic path to silent corruption.
func (s *Store) get(key []byte) ([]byte, error) {
	v, closer, err := s.db.Get(key)
	if errors.Is(err, pebble.ErrNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	out := make([]byte, len(v))
	copy(out, v)
	return out, closer.Close()
}

// OpenMem creates the storage in memory. It is for tests — the data does not outlive the process.
//
// It is not a shortcut for production: a node without a datadir would resync the whole chain after a
// restart, and that is exactly what this package is about.
func OpenMem() (*Store, error) { return OpenMemWith(Options{}) }

// OpenMemWith is OpenMem with options — for the index tests.
func OpenMemWith(opt Options) (*Store, error) {
	db, err := pebble.Open("", &pebble.Options{FS: vfs.NewMem()})
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	s := &Store{db: db, indexTx: opt.TxIndex, indexAddr: opt.AddrIndex}
	if err := s.checkSchema(); err != nil {
		db.Close()
		return nil, err
	}
	if err := s.ensureCreditIndex(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}
