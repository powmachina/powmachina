package store

import (
	"testing"
)

// The mark on an invalid block has to survive closing the storage and the body has to disappear —
// otherwise the node hands it out to peers on request and would be spreading an invalid block.
func TestMarkInvalidPersistsAndDropsBody(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	blocks := testChain(3)
	if err := s.PutBlock(blocks[2]); err != nil {
		t.Fatal(err)
	}
	id := blocks[2].Header.ID()
	if !s.HasBody(id) {
		t.Fatal("setup: the body was not stored")
	}

	if err := s.MarkInvalid(id); err != nil {
		t.Fatal(err)
	}
	if !s.IsInvalid(id) {
		t.Fatal("the block is not marked")
	}
	if s.HasBody(id) {
		t.Fatal("the invalid block's body stayed in the storage")
	}
	ids, err := s.InvalidIDs()
	if err != nil || len(ids) != 1 || ids[0] != id {
		t.Fatalf("the list of marked blocks: %d (%v)", len(ids), err)
	}
	s.Close()

	again, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if !again.IsInvalid(id) {
		t.Fatal("the mark did not survive closing the storage")
	}
	// The header stays — the node refuses the header by it without downloading the body.
	if _, err := again.Header(id); err != nil {
		t.Fatalf("the header should not have been deleted: %v", err)
	}
}

// Clean storage has nothing marked and the list is empty, not an error.
func TestInvalidIDsEmpty(t *testing.T) {
	s, err := OpenMem()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ids, err := s.InvalidIDs()
	if err != nil || len(ids) != 0 {
		t.Fatalf("expected an empty list, got %d (%v)", len(ids), err)
	}
}
