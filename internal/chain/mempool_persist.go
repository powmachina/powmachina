package chain

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"
)

// Surviving the mempool across a restart.
//
// The node dumps the mempool on shutdown and reads it at startup. Without that every restart
// throws away transactions that are not in a block yet — and whoever sent them does not know:
// their wallet already says "sent", but the network has forgotten the transaction.
//
// Loaded transactions get **no privilege**: each one goes through `Accept` again, through the same
// signatures, fees, dust limit and chaining as a transaction from the network. The file is input
// from an attacker like any other — it may be planted, corrupted or from another network.

const (
	// MempoolExpiry is how long a transaction may wait in the pool. Bitcoin has two weeks; less is
	// enough for us, because without expiry a dumped mempool would turn one stuck transaction into
	// permanent litter carried across every restart.
	MempoolExpiry = 72 * time.Hour
	// MaxMempoolDumpTxs is the cap on how many transactions are loaded from the file. The same as
	// the pool's memory cap — the file must not be able to make a bigger pool than the network can.
	MaxMempoolDumpTxs = MaxMempoolTxs
)

// mempoolDumpMagic carries the format version too. When the format changes, an old file is thrown
// away — the mempool is not data that can be lost, and a migration would cost more than an hour of
// lost transactions.
var mempoolDumpMagic = [8]byte{'P', 'O', 'W', 'M', 'M', 'P', '0', '1'}

// ErrMempoolDumpFormat means the file is not a mempool dump of this version.
var ErrMempoolDumpFormat = errors.New("mempool: the file is not a mempool dump in a known format")

// Dump writes the mempool out. It returns the number of transactions written.
//
// The order is by the time of acceptance, so that on loading a parent comes before its child —
// otherwise the child would look like an orphan the first time and the whole chain would be lost.
func (m *Mempool) Dump(w io.Writer) (int, error) {
	m.mu.RLock()
	list := make([]*MempoolEntry, 0, len(m.entries))
	for _, e := range m.entries {
		list = append(list, e)
	}
	m.mu.RUnlock()

	// Ordered by the time of acceptance; with equal times by txid, so the dump is repeatable and
	// can be compared byte for byte.
	sort.Slice(list, func(i, j int) bool {
		if !list[i].Added.Equal(list[j].Added) {
			return list[i].Added.Before(list[j].Added)
		}
		return string(list[i].TxID[:]) < string(list[j].TxID[:])
	})

	var hdr [12]byte
	copy(hdr[:8], mempoolDumpMagic[:])
	binary.LittleEndian.PutUint32(hdr[8:], uint32(len(list)))
	if _, err := w.Write(hdr[:]); err != nil {
		return 0, err
	}
	for _, e := range list {
		raw := e.Tx.Serialize()
		var rec [12]byte
		binary.LittleEndian.PutUint64(rec[0:], uint64(e.Added.Unix()))
		binary.LittleEndian.PutUint32(rec[8:], uint32(len(raw)))
		if _, err := w.Write(rec[:]); err != nil {
			return 0, err
		}
		if _, err := w.Write(raw); err != nil {
			return 0, err
		}
	}
	return len(list), nil
}

// Load reads the dump and lets every transaction go through `Accept`. It returns the number of
// accepted and the number of dropped transactions.
//
// An error is only returned when the file cannot be read — a single rejected transaction is not an
// error. Over the 72 hours the file may have been lying around, anything could have been confirmed.
func (m *Mempool) Load(r io.Reader, utxo CoinView, height uint64, now time.Time) (int, int, error) {
	var hdr [12]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, 0, fmt.Errorf("%w: %v", ErrMempoolDumpFormat, err)
	}
	if [8]byte(hdr[0:8]) != mempoolDumpMagic {
		return 0, 0, ErrMempoolDumpFormat
	}
	count := binary.LittleEndian.Uint32(hdr[8:])
	if count > MaxMempoolDumpTxs {
		return 0, 0, fmt.Errorf("%w: it reports %d transactions (cap %d)", ErrMempoolDumpFormat,
			count, MaxMempoolDumpTxs)
	}

	cutoff := now.Add(-MempoolExpiry)
	var loaded, dropped int
	for i := uint32(0); i < count; i++ {
		var rec [12]byte
		if _, err := io.ReadFull(r, rec[:]); err != nil {
			return loaded, dropped, fmt.Errorf("%w: %v", ErrMempoolDumpFormat, err)
		}
		size := binary.LittleEndian.Uint32(rec[8:])
		// A cap per transaction: without it the header could request a gigabyte of memory.
		if size == 0 || size > MaxBlockBytes {
			return loaded, dropped, fmt.Errorf("%w: a transaction reports %d bytes",
				ErrMempoolDumpFormat, size)
		}
		raw := make([]byte, size)
		if _, err := io.ReadFull(r, raw); err != nil {
			return loaded, dropped, fmt.Errorf("%w: %v", ErrMempoolDumpFormat, err)
		}
		added := time.Unix(int64(binary.LittleEndian.Uint64(rec[0:])), 0)
		if added.Before(cutoff) {
			dropped++
			continue
		}
		tx, used, err := DeserializeTx(raw)
		if err != nil || used != len(raw) {
			dropped++
			continue
		}
		e, err := m.Accept(tx, utxo, height)
		if err != nil {
			dropped++
			continue
		}
		// The time of acceptance is carried over, otherwise a restart would make the whole pool
		// young again and expiry would never be reached. A time from the future is ignored — the
		// file does not get to dictate that.
		if added.Before(now) {
			m.setAdded(e.TxID, added)
		}
		loaded++
	}
	return loaded, dropped, nil
}

// setAdded overwrites the time of acceptance. Only loading a dump uses it.
func (m *Mempool) setAdded(id [32]byte, t time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.entries[id]; ok {
		e.Added = t
	}
}

// ExpireBefore throws out the transactions accepted before `cutoff` — along with their
// descendants, because a child without its parent is invalid. It returns the number removed.
func (m *Mempool) ExpireBefore(cutoff time.Time) int {
	m.mu.Lock()
	defer m.mu.Unlock()

	var old [][32]byte
	for id, e := range m.entries {
		if e.Added.Before(cutoff) {
			old = append(old, id)
		}
	}
	before := len(m.entries)
	for _, id := range old {
		m.removeTreeLocked(id)
	}
	return before - len(m.entries)
}
