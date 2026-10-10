package store

import (
	"fmt"

	"github.com/cockroachdb/pebble/v2"
)

// Marking invalid blocks.
//
// A block whose body failed validation into the UTXO set is invalid as a whole — and with it the
// whole branch above it (`chain/invalid.go`). The mark has to survive a restart: otherwise after
// startup the node would accept the same header again, download the body again and stall again.

// MarkInvalid marks a block invalid and throws away its body.
//
// The body is deleted for two reasons: it has nothing to be there for (it will never be applied) and
// otherwise the node hands it out to peers on request — it would be spreading an invalid block.
func (s *Store) MarkInvalid(id [32]byte) error {
	batch := s.db.NewBatch()
	defer batch.Close()
	if err := batch.Set(keyWithID(prefixInvalid, id), nil, nil); err != nil {
		return fmt.Errorf("store: %w", err)
	}
	if err := batch.Delete(keyWithID(prefixBody, id), nil); err != nil {
		return fmt.Errorf("store: %w", err)
	}
	return batch.Commit(pebble.Sync)
}

// IsInvalid says whether a block is marked invalid.
func (s *Store) IsInvalid(id [32]byte) bool {
	_, err := s.get(keyWithID(prefixInvalid, id))
	return err == nil
}

// InvalidIDs returns every marked block — the restore reads them at startup.
func (s *Store) InvalidIDs() ([][32]byte, error) {
	lo := []byte{prefixInvalid}
	hi := []byte{prefixInvalid + 1}
	it, err := s.db.NewIter(&pebble.IterOptions{LowerBound: lo, UpperBound: hi})
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	defer it.Close()

	var out [][32]byte
	for it.First(); it.Valid(); it.Next() {
		k := it.Key()
		if len(k) != 33 {
			return nil, fmt.Errorf("store: a corrupted key of an invalid block (%d B)", len(k))
		}
		var id [32]byte
		copy(id[:], k[1:])
		out = append(out, id)
	}
	return out, it.Error()
}
