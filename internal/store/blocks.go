package store

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/cockroachdb/pebble/v2"
	"powmachina/internal/chain"
)

// Tip is the tip of the active branch as the storage sees it. It is **at the same time** the mark
// the UTXO set is applied up to — and that is intended: since both change in one atomic batch, they
// cannot diverge.
type Tip struct {
	ID     [32]byte
	Height uint64
}

func encodeTip(t Tip) []byte {
	b := make([]byte, 32+8)
	copy(b, t.ID[:])
	binary.BigEndian.PutUint64(b[32:], t.Height)
	return b
}

func decodeTip(b []byte) (Tip, error) {
	if len(b) != 40 {
		return Tip{}, fmt.Errorf("store: a corrupted tip mark (%d B)", len(b))
	}
	var t Tip
	copy(t.ID[:], b[:32])
	t.Height = binary.BigEndian.Uint64(b[32:])
	return t, nil
}

// Tip returns the current tip. When the storage has none yet, it returns ErrNotFound.
func (s *Store) Tip() (Tip, error) {
	v, err := s.get(keyTip)
	if err != nil {
		return Tip{}, err
	}
	return decodeTip(v)
}

// PutHeader stores a verified header. It does **not** file it into the height index.
//
// Why not: a header by itself does not say whether it is on the active branch. When PutHeader wrote
// the index, one header from a branch that lost was enough and the index at that height pointed at a
// block outside the chain — and `Restore` then loaded, from that index, a chain assembled out of two
// branches after a restart. The index is set by `SetActiveBranch`, called by whoever knows what the
// branch looks like (HeaderChain).
func (s *Store) PutHeader(h *chain.Header) error {
	id := h.ID()
	hdr := h.Serialize()
	batch := s.db.NewBatch()
	defer batch.Close()
	if err := batch.Set(keyWithID(prefixHeader, id), hdr[:], nil); err != nil {
		return fmt.Errorf("store: %w", err)
	}
	return batch.Commit(pebble.Sync)
}

// BranchEntry is one height of the active branch.
type BranchEntry struct {
	Height uint64
	ID     [32]byte
}

// SetActiveBranch rewrites the height index so it describes the active branch, and moves the header
// mark to its tip.
//
// `entries` have to go in ascending order and end with the branch's tip. Entries above the tip are
// discarded — they come from a branch that lost, and if they stayed, `Restore` would load headers
// from two branches at once after a restart.
//
// The whole thing is one atomic batch: after a crash in the middle, an index mixing two branches
// cannot arise. That matters more than it seems — the index is the manifest the bodies are filled in
// by and the chain is restored from.
func (s *Store) SetActiveBranch(entries []BranchEntry) error {
	if len(entries) == 0 {
		return nil
	}
	top := entries[len(entries)-1]
	batch := s.db.NewBatch()
	defer batch.Close()
	for i, e := range entries {
		if i > 0 && e.Height != entries[i-1].Height+1 {
			return fmt.Errorf("store: a gap in the active branch between %d and %d", entries[i-1].Height, e.Height)
		}
		if err := batch.Set(keyHeight(e.Height), e.ID[:], nil); err != nil {
			return fmt.Errorf("store: %w", err)
		}
	}
	for h := top.Height + 1; ; h++ {
		if _, err := s.get(keyHeight(h)); errors.Is(err, ErrNotFound) {
			break
		} else if err != nil {
			return err
		}
		if err := batch.Delete(keyHeight(h), nil); err != nil {
			return fmt.Errorf("store: %w", err)
		}
	}
	if err := batch.Set(keyHeaderTip, encodeTip(Tip{ID: top.ID, Height: top.Height}), nil); err != nil {
		return fmt.Errorf("store: %w", err)
	}
	return batch.Commit(pebble.Sync)
}

// HeaderTip is the highest verified header. It can be above Tip — the bodies may not be there yet.
func (s *Store) HeaderTip() (Tip, error) {
	v, err := s.get(keyHeaderTip)
	if err != nil {
		return Tip{}, err
	}
	return decodeTip(v)
}

// PutBlock stores the header and the body. It does **not** apply it — ApplyBlock is for that.
//
// The split is deliberate: downloading bodies can run in parallel and out of order, applying has to
// be serial. A block stored and not applied is a legitimate state that is settled at startup by
// replaying, without anything being pulled from the network.
func (s *Store) PutBlock(b *chain.Block) error {
	id := b.Header.ID()
	hdr := b.Header.Serialize()

	batch := s.db.NewBatch()
	defer batch.Close()
	if err := batch.Set(keyWithID(prefixHeader, id), hdr[:], nil); err != nil {
		return fmt.Errorf("store: %w", err)
	}
	if err := batch.Set(keyWithID(prefixBody, id), b.Serialize(), nil); err != nil {
		return fmt.Errorf("store: %w", err)
	}
	// Sync: the body has to be on disk before applying leans on it.
	return batch.Commit(pebble.Sync)
}

func (s *Store) HasBlock(id [32]byte) bool {
	_, err := s.get(keyWithID(prefixHeader, id))
	return err == nil
}

func (s *Store) Header(id [32]byte) (*chain.Header, error) {
	v, err := s.get(keyWithID(prefixHeader, id))
	if err != nil {
		return nil, err
	}
	return chain.DeserializeHeader(v)
}

func (s *Store) Block(id [32]byte) (*chain.Block, error) {
	v, err := s.get(keyWithID(prefixBody, id))
	if err != nil {
		return nil, err
	}
	return chain.DeserializeBlock(v)
}

// BlockIDAt returns the id of the block at a given height of the active branch.
func (s *Store) BlockIDAt(height uint64) ([32]byte, error) {
	var id [32]byte
	v, err := s.get(keyHeight(height))
	if err != nil {
		return id, err
	}
	if len(v) != 32 {
		return id, fmt.Errorf("store: a corrupted index of height %d", height)
	}
	copy(id[:], v)
	return id, nil
}

// MissingBodies returns the heights that are in the index but whose body is missing. It is used at
// startup to find out what has to be pulled.
func (s *Store) MissingBodies(from, to uint64) ([]uint64, error) {
	var out []uint64
	for h := from; h <= to; h++ {
		id, err := s.BlockIDAt(h)
		if errors.Is(err, ErrNotFound) {
			out = append(out, h)
			continue
		}
		if err != nil {
			return nil, err
		}
		if _, err := s.get(keyWithID(prefixBody, id)); errors.Is(err, ErrNotFound) {
			out = append(out, h)
		} else if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// HasBody says whether we have a block's body (not just the header).
func (s *Store) HasBody(id [32]byte) bool {
	_, err := s.get(keyWithID(prefixBody, id))
	return err == nil
}
