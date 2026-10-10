package store

import (
	"errors"
	"fmt"
	"log"

	"github.com/cockroachdb/pebble/v2"

	"powmachina/internal/chain"
)

// Restore restores the node's state from disk. This is the function that decides whether the node
// starts in seconds after a crash or starts resyncing from the network.
//
// It does three things:
//
//  1. It reads the tip mark. Since it changes in the same atomic batch as the UTXO set, it is a
//     **reliable statement of the block the UTXO set is applied up to** — not an estimate.
//  2. It loads the headers from the genesis up to the tip into `HeaderChain`, without recomputing
//     PoW (see `chain.HeaderChain.LoadHeader`).
//  3. It applies the blocks that are stored but not applied yet. Those are on disk, so nothing is
//     pulled from the network.
//
// It returns the height the node continues at.
func (s *Store) Restore(hc *chain.HeaderChain) (uint64, error) {
	// The headers are loaded up to **headerTip**, not up to tip: there can be more headers than
	// applied blocks and without them the node would not know which bodies it is missing.
	top, herr := s.HeaderTip()
	if herr != nil && !errors.Is(herr, ErrNotFound) {
		return 0, herr
	}
	applied, terr := s.Tip()
	if terr != nil && !errors.Is(terr, ErrNotFound) {
		return 0, terr
	}
	if herr != nil && terr != nil {
		return 0, nil // fresh storage
	}
	// The higher of the two marks is taken, not just headerTip. A datadir whose header mark lags
	// behind the applied tip must not make the node report height 0 here and throw away a chain it
	// has in full on disk.
	tip := top
	if herr != nil || (terr == nil && applied.Height > top.Height) {
		tip = applied
	}

	// The genesis is already in HeaderChain from the constructor; we only check that it matches the storage.
	genesisID, err := s.BlockIDAt(0)
	if err != nil {
		return 0, fmt.Errorf("store: the storage has no genesis: %w", err)
	}
	if !hc.Has(genesisID) {
		return 0, fmt.Errorf("store: the genesis in the storage (%x) does not belong to this network — "+
			"probably a datadir from another network", genesisID[:6])
	}

	// The genesis is in HeaderChain from the constructor, we load from height 1.
	for h := uint64(1); h <= tip.Height; h++ {
		id, err := s.BlockIDAt(h)
		if err != nil {
			return 0, fmt.Errorf("store: the index of height %d is missing (the tip is %d): %w", h, tip.Height, err)
		}
		hdr, err := s.Header(id)
		if err != nil {
			return 0, fmt.Errorf("store: the header at height %d is missing: %w", h, err)
		}
		if err := hc.LoadHeader(hdr); err != nil {
			switch {
			case errors.Is(err, chain.ErrDuplicate):
			case errors.Is(err, chain.ErrInvalidAncestor):
				// The index points at a branch the node has marked invalid in the meantime — that
				// happens when it crashes between the marking and rewriting the index. We load the
				// chain up to this height; the node pulls the rest from the network again.
				log.Printf("restore: height %d is on an invalid branch, the chain loaded up to %d", h, h-1)
				return h - 1, nil
			default:
				return 0, fmt.Errorf("store: the header at height %d cannot be loaded: %w", h, err)
			}
		}
	}
	return tip.Height, nil
}

// ApplyHooks are the hooks of the serial application of stored bodies.
type ApplyHooks struct {
	// Validate is called before a block is applied; an error stops applying.
	Validate func(*chain.Block) error
	// Applied is called after a block has been applied successfully.
	Applied func(*chain.Block)
}

// ApplyStored applies the blocks that are stored on disk but not reflected in the UTXO set.
//
// It comes about because `PutBlock` and `ApplyBlockDirect` are deliberately two operations: bodies
// can be downloaded in parallel and out of order, applying has to be serial.
//
// # Why validation MUST be here
//
// Above the tip there may be bodies nobody has verified yet — the parallel downloader stores them as
// soon as their hash matches, but **they can only be verified against the UTXO set in order**. The
// invariant is therefore: *blocks up to the tip's height are verified, bodies above the tip are not.*
//
// That is why `Validate` is called on every block before it is applied. The caller puts
// `chain.Block.ValidateBlock` there. An empty `Validate` is only allowed where it is certain that
// nobody planted the bodies (tests).
//
// `Applied` is called after every successfully applied block — the caller puts there what belongs to
// a new tip and not in storage: cleaning the mempool, the log, the finality attestation.
//
// It returns the number of blocks applied.
func (s *Store) ApplyStored(upTo uint64, h ApplyHooks) (int, error) {
	n := 0
	for {
		tip, err := s.Tip()
		next := uint64(0)
		switch {
		case errors.Is(err, ErrNotFound):
			next = 0
		case err != nil:
			return n, err
		default:
			next = tip.Height + 1
		}
		if next > upTo {
			return n, nil
		}
		id, err := s.BlockIDAt(next)
		if errors.Is(err, ErrNotFound) {
			return n, nil // the index does not reach further, there is nothing to apply
		}
		if err != nil {
			return n, err
		}
		b, err := s.Block(id)
		if errors.Is(err, ErrNotFound) {
			return n, nil // we do not have the body yet, it will be filled in from the network
		}
		if err != nil {
			return n, err
		}
		if h.Validate != nil {
			if err := h.Validate(b); err != nil {
				return n, fmt.Errorf("store: the block at height %d failed validation: %w", next, err)
			}
		}
		if _, err := s.ApplyBlockDirect(b); err != nil {
			return n, fmt.Errorf("store: applying height %d failed: %w", next, err)
		}
		n++
		if h.Applied != nil {
			h.Applied(b)
		}
	}
}

// InitGenesis writes the genesis into empty storage: the header, the index of height 0 and the tip mark.
//
// The genesis is **only a header** — it has no body and no transactions, so it does not change the
// UTXO set. It is the root of trust written into the binary, not a block received from the network,
// and that is why it is not verified either.
//
// On already initialized storage it does nothing, it only checks that the genesis matches. A
// different genesis means a datadir from another network and it is a reason to refuse to start, not
// to overwrite it.
func (s *Store) InitGenesis(h *chain.Header) error {
	id := h.ID()
	if existing, err := s.BlockIDAt(0); err == nil {
		if existing != id {
			return fmt.Errorf("store: the datadir belongs to another network (genesis %x, expected %x)",
				existing[:6], id[:6])
		}
		return nil
	} else if !errors.Is(err, ErrNotFound) {
		return err
	}

	hdr := h.Serialize()
	batch := s.db.NewBatch()
	defer batch.Close()
	if err := batch.Set(keyWithID(prefixHeader, id), hdr[:], nil); err != nil {
		return fmt.Errorf("store: %w", err)
	}
	if err := batch.Set(keyHeight(0), id[:], nil); err != nil {
		return fmt.Errorf("store: %w", err)
	}
	gt := encodeTip(Tip{ID: id, Height: 0})
	if err := batch.Set(keyTip, gt, nil); err != nil {
		return fmt.Errorf("store: %w", err)
	}
	if err := batch.Set(keyHeaderTip, gt, nil); err != nil {
		return fmt.Errorf("store: %w", err)
	}
	return batch.Commit(pebble.Sync)
}
