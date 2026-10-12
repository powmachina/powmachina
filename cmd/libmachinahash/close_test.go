package main

import (
	"sync"
	"sync/atomic"
	"testing"

	"powmachina/internal/mh"
)

// A pool closes the context of an epoch that has ended while its other threads are still hashing
// shares of that epoch. A thread that found the context a moment before it was closed must get an
// error code back. Hashing on what the close has freed is a crash of the process the library is
// loaded in, and that process is the pool.
func TestHashOnAContextClosedMeanwhileIsAnError(t *testing.T) {
	var hdr [mh.HeaderBytes]byte

	t.Run("lane G", func(t *testing.T) {
		for _, full := range []bool{false, true} {
			h := openContext(0, full, false, "regtest")
			if h == 0 {
				t.Fatal("the context could not be opened")
			}
			ctx, ok := lookup(h)
			if !ok {
				t.Fatal("the context is not registered")
			}
			if _, code := ctx.hash(1, &hdr, 1); code != errOK {
				t.Fatalf("full=%v: a hash on an open context returned %d", full, code)
			}
			closeContext(h)
			if _, code := ctx.hash(1, &hdr, 1); code != errNoContext {
				t.Fatalf("full=%v: a hash on a closed context returned %d, expected %d", full, code, errNoContext)
			}
		}
	})

	t.Run("lane C", func(t *testing.T) {
		h := openContext(0, false, true, "regtest")
		if h == 0 {
			t.Skip("RandomX could not be initialised here")
		}
		ctx, _ := lookup(h)
		if _, code := ctx.hash(0, &hdr, 1); code != errOK {
			t.Fatalf("a hash on an open context returned %d", code)
		}
		closeContext(h)
		// The pool of hashers is empty after the close, so this call would build a RandomX VM on a
		// context that no longer exists.
		if _, code := ctx.hash(0, &hdr, 2); code != errNoContext {
			t.Fatalf("a hash on a closed context returned %d, expected %d", code, errNoContext)
		}
	})
}

// Closing waits for the hashes in flight and every hash either finishes on a live context or is
// refused. Run under the race detector this is also the check that a close and a hash never touch
// the same memory at once.
func TestCloseWhileHashing(t *testing.T) {
	var hdr [mh.HeaderBytes]byte
	for _, full := range []bool{false, true} {
		h := openContext(0, full, false, "regtest")
		if h == 0 {
			t.Fatal("the context could not be opened")
		}
		want, code := mustLookup(t, h).hash(1, &hdr, 7)
		if code != errOK {
			t.Fatalf("the reference hash returned %d", code)
		}

		var wg sync.WaitGroup
		var done, refused atomic.Int64
		started := make(chan struct{})
		for w := 0; w < 8; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-started
				for i := 0; i < 200; i++ {
					ctx, ok := lookup(h)
					if !ok {
						refused.Add(1)
						continue
					}
					got, code := ctx.hash(1, &hdr, 7)
					switch {
					case code == errNoContext:
						refused.Add(1)
					case code != errOK:
						t.Errorf("full=%v: a hash returned %d", full, code)
					case got != want:
						t.Errorf("full=%v: a hash computed during a close is wrong", full)
					default:
						done.Add(1)
					}
				}
			}()
		}
		close(started)
		closeContext(h)
		closeContext(h) // a second close is a no-op
		wg.Wait()
		if refused.Load() == 0 {
			t.Errorf("full=%v: no hash was refused, the close came after all of them", full)
		}
	}
}

func mustLookup(t *testing.T, h uint64) *context {
	t.Helper()
	ctx, ok := lookup(h)
	if !ok {
		t.Fatal("the context is not registered")
	}
	return ctx
}
