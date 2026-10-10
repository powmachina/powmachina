package store

import (
	"errors"
	"testing"
)

// SetActiveBranch has to rewrite the index onto the new branch and discard the entries above its tip.
// If they stayed, Restore would load headers from two branches at once after a restart.
func TestSetActiveBranchReplacesAndTruncates(t *testing.T) {
	s, err := OpenMem()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	id := func(b byte) [32]byte {
		var out [32]byte
		out[0] = b
		return out
	}

	// The longer branch: heights 1..4.
	if err := s.SetActiveBranch([]BranchEntry{
		{Height: 1, ID: id(1)}, {Height: 2, ID: id(2)},
		{Height: 3, ID: id(3)}, {Height: 4, ID: id(4)},
	}); err != nil {
		t.Fatal(err)
	}
	if tip, err := s.HeaderTip(); err != nil || tip.Height != 4 || tip.ID != id(4) {
		t.Fatalf("the header mark: %+v, %v", tip, err)
	}

	// A reorg onto a shorter but heavier branch: height 3 is a different block and 4 no longer exists.
	if err := s.SetActiveBranch([]BranchEntry{{Height: 3, ID: id(0x33)}}); err != nil {
		t.Fatal(err)
	}
	got, err := s.BlockIDAt(3)
	if err != nil || got != id(0x33) {
		t.Fatalf("height 3 should be the new branch, it is %x (%v)", got[:4], err)
	}
	if _, err := s.BlockIDAt(4); !errors.Is(err, ErrNotFound) {
		t.Fatalf("height 4 should have disappeared, the error is %v", err)
	}
	if tip, err := s.HeaderTip(); err != nil || tip.Height != 3 || tip.ID != id(0x33) {
		t.Fatalf("the header mark did not come back to 3: %+v, %v", tip, err)
	}
	// The common base must not be touched.
	if got, err := s.BlockIDAt(1); err != nil || got != id(1) {
		t.Fatalf("the common base changed: %x (%v)", got[:4], err)
	}
}

// A gap in the input is the caller's bug — the index has to be contiguous, otherwise the chain cannot
// be walked by it.
func TestSetActiveBranchRejectsGap(t *testing.T) {
	s, err := OpenMem()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var a, b [32]byte
	a[0], b[0] = 1, 2
	if err := s.SetActiveBranch([]BranchEntry{{Height: 1, ID: a}, {Height: 3, ID: b}}); err == nil {
		t.Fatal("a gap between heights should have been refused")
	}
}
