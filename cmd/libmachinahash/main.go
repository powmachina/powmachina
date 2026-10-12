// libmachinahash — MachinaHash as a shared library with a plain C ABI.
//
//	go build -buildmode=c-shared -o libmachinahash.so ./cmd/libmachinahash
//
// It exists for **one** reason: a pool has to verify shares itself, and project rule 1 says the
// algorithm has one implementation. Miningcore is C# and cannot call `internal/mh` directly, so
// instead of a second implementation ("just for verifying") it calls this library through P/Invoke.
// What runs in the pool is therefore our code, not a copy of it — a difference of one constant
// between the two would be a network fork, and it would show up as the pool paying for shares the
// network refuses.
//
// The same library serves anyone else who needs the hash outside Go: an explorer, a test harness,
// another pool. The interface is deliberately narrow — a context per epoch, and a hash.
//
// Threading: a context may be used from several threads at once. Lane G is stateless beyond the DAG
// and takes a scratch buffer per call; lane C needs a RandomX VM per thread, so the context holds a
// small pool of them (see vmFor). Nothing here keeps per-call state in a global.
package main

/*
#include <stdlib.h>
*/
import "C"

import (
	"sync"
	"unsafe"

	"powmachina/internal/mh"
)

// Error codes. Zero is success; the caller is expected to treat anything else as "do not trust this
// share". A message is available through mh_last_error so the operator sees a reason and not a number.
const (
	errOK         = 0
	errNoContext  = -1
	errBadLane    = -2
	errHashFailed = -3
	errBadArgs    = -4
)

type context struct {
	epoch    uint64
	verifier *mh.Verifier
	randomx  *mh.RandomX

	// life ties a hash in flight to the context it runs on. A caller closes a context from one
	// thread (the epoch turned) while others are still hashing shares of that epoch on it, and
	// closing frees what they are reading: the RandomX dataset is C memory, the DAG is dropped. So
	// every hash holds life for reading, and closing takes it for writing — it waits for the hashes
	// in flight and marks the context closed for the ones that found it a moment earlier.
	life   sync.RWMutex
	closed bool

	mu      sync.Mutex
	hashers []*mh.Hasher // one per thread that asked, reused
}

var (
	registry   sync.Map // handle -> *context
	nextHandle uint64   = 1
	handleMu   sync.Mutex

	lastErrMu sync.Mutex
	lastErr   string
)

func setErr(s string) {
	lastErrMu.Lock()
	lastErr = s
	lastErrMu.Unlock()
}

//export mh_abi
func mh_abi() C.int { return 1 }

//export mh_header_bytes
func mh_header_bytes() C.int { return C.int(mh.HeaderBytes) }

// mh_last_error returns the last error as a NUL-terminated string. The caller must not free it; it
// stays valid until the next failing call on any thread.
//
//export mh_last_error
func mh_last_error() *C.char {
	lastErrMu.Lock()
	defer lastErrMu.Unlock()
	if lastErr == "" {
		return nil
	}
	return C.CString(lastErr)
}

// mh_free releases a string handed out by mh_last_error.
//
//export mh_free
func mh_free(p *C.char) { C.free(unsafe.Pointer(p)) }

// mh_open builds a context for one epoch. `full` picks the full DAG (a pool wants it: 0.41 ms per
// lane G share against ~63 ms in light mode) and `randomx` says whether lane C will be hashed —
// building the RandomX dataset costs memory and seconds, so a lane G pool does not pay for it.
//
// The DAG profile comes from the **network**, not from a flag, because that is exactly the parameter
// a caller cannot be trusted to supply: regtest and the test vectors use a 1 MiB cache while mainnet
// uses the full one, and a context opened with the wrong profile hashes everything differently. It
// does not fail, it just disagrees with the network — the first round-trip test written against this
// library hit it immediately, submitted a block and got "the header does not meet its target" back
// from the node. So the caller names the network and `params.DagProfile` decides, the same field the
// node itself uses.
//
// The handle it returns is opaque and must be closed with mh_close.
//
//export mh_open
func mh_open(epoch C.ulonglong, full C.int, randomx C.int, network *C.char) C.ulonglong {
	if network == nil {
		setErr("network is a null pointer")
		return 0
	}
	return C.ulonglong(openContext(uint64(epoch), full != 0, randomx != 0, C.GoString(network)))
}

// openContext is mh_open without the C types, so it can be called from a test.
func openContext(epoch uint64, full, withRandomX bool, netName string) uint64 {
	params, ok := networkMap[netName]
	if !ok {
		setErr("unknown network " + netName + " (expected mainnet, testnet or regtest)")
		return 0
	}
	profile := params.DagProfile
	// The epoch is the caller's number, and it sizes everything built below.
	if !mh.ValidEpoch(epoch) {
		setErr("the epoch is past the last one a chain can reach")
		return 0
	}

	ctx := &context{epoch: epoch}
	if full {
		ctx.verifier = mh.NewVerifier(epoch, profile)
	} else {
		ctx.verifier = mh.NewLightVerifier(epoch, profile)
	}
	if withRandomX {
		rx, err := mh.NewRandomX(epoch, full)
		if err != nil {
			ctx.verifier.Close()
			setErr("RandomX for epoch could not be initialised: " + err.Error())
			return 0
		}
		ctx.randomx = rx
	}

	handleMu.Lock()
	h := nextHandle
	nextHandle++
	handleMu.Unlock()
	registry.Store(h, ctx)
	return h
}

// mh_close frees a context. It may be called while other threads are hashing on the same handle:
// it returns once the hashes in flight have finished, and a hash that starts after it answers
// "unknown context handle". Closing a handle twice, or one that never existed, does nothing.
//
//export mh_close
func mh_close(handle C.ulonglong) { closeContext(uint64(handle)) }

func closeContext(handle uint64) {
	v, ok := registry.LoadAndDelete(handle)
	if !ok {
		return
	}
	ctx := v.(*context)
	ctx.life.Lock()
	defer ctx.life.Unlock()
	ctx.closed = true
	ctx.mu.Lock()
	for _, h := range ctx.hashers {
		h.Close()
	}
	ctx.hashers = nil
	ctx.mu.Unlock()
	if ctx.randomx != nil {
		ctx.randomx.Close()
	}
	ctx.verifier.Close()
}

// hasherFor takes a hasher out of the context's pool, or builds one. A hasher owns a RandomX VM, so
// it is not free to create — but it also must not be shared between threads at the same time.
func (c *context) hasherFor() (*mh.Hasher, error) {
	c.mu.Lock()
	if n := len(c.hashers); n > 0 {
		h := c.hashers[n-1]
		c.hashers = c.hashers[:n-1]
		c.mu.Unlock()
		return h, nil
	}
	c.mu.Unlock()
	if c.randomx != nil {
		return mh.NewHasherWithRandomX(c.randomx)
	}
	return mh.NewHasher(), nil
}

func (c *context) put(h *mh.Hasher) {
	c.mu.Lock()
	// A pool has a bounded number of worker threads; the cap is only here so a burst of threads
	// cannot pin memory forever.
	if len(c.hashers) < 64 {
		c.hashers = append(c.hashers, h)
		c.mu.Unlock()
		return
	}
	c.mu.Unlock()
	h.Close()
}

// mh_hash computes one PoW hash. header is 80 bytes without the nonce, out is 32 bytes of space the
// caller owns. lane is 0 for C and 1 for G, matching mh.LaneC/mh.LaneG.
//
//export mh_hash
func mh_hash(handle C.ulonglong, lane C.int, header *C.uchar, nonce C.ulonglong, out *C.uchar) C.int {
	if header == nil || out == nil {
		setErr("header or out is a null pointer")
		return errBadArgs
	}
	ctx, ok := lookup(uint64(handle))
	if !ok {
		setErr("unknown context handle")
		return errNoContext
	}
	var hdr [mh.HeaderBytes]byte
	copy(hdr[:], unsafe.Slice((*byte)(unsafe.Pointer(header)), mh.HeaderBytes))

	sum, code := ctx.hash(int(lane), &hdr, uint64(nonce))
	if code != errOK {
		return C.int(code)
	}
	copy(unsafe.Slice((*byte)(unsafe.Pointer(out)), 32), sum[:])
	return errOK
}

func lookup(handle uint64) (*context, bool) {
	v, ok := registry.Load(handle)
	if !ok {
		return nil, false
	}
	return v.(*context), true
}

// hash computes one PoW hash on the context and returns it with an error code.
//
// It holds the context for the whole computation. A context that was closed between the caller
// finding it and getting here is answered like a handle that does not exist — to the caller it is
// the same thing, and what must never happen is hashing on memory that has been freed: that is not
// an error code, it brings down the process the library is loaded in.
func (c *context) hash(lane int, hdr *[mh.HeaderBytes]byte, nonce uint64) ([32]byte, int) {
	c.life.RLock()
	defer c.life.RUnlock()
	if c.closed {
		setErr("unknown context handle")
		return [32]byte{}, errNoContext
	}

	var l mh.Lane
	switch lane {
	case 0:
		l = mh.LaneC
		if c.randomx == nil {
			setErr("this context was opened without RandomX, so it cannot hash lane C")
			return [32]byte{}, errBadLane
		}
	case 1:
		l = mh.LaneG
	default:
		setErr("lane has to be 0 (C) or 1 (G)")
		return [32]byte{}, errBadLane
	}

	h, err := c.hasherFor()
	if err != nil {
		setErr("a hasher could not be created: " + err.Error())
		return [32]byte{}, errHashFailed
	}
	sum, err := h.Hash(c.verifier, hdr, nonce, l)
	c.put(h)
	if err != nil {
		setErr("the hash failed: " + err.Error())
		return [32]byte{}, errHashFailed
	}
	return sum, errOK
}

// mh_epoch_for_height is here so the caller does not have to repeat the epoch length and get it
// wrong. An epoch boundary with the wrong DAG means every share looks invalid.
//
//export mh_epoch_for_height
func mh_epoch_for_height(height C.ulonglong) C.ulonglong {
	return C.ulonglong(mh.EpochForHeight(uint64(height)))
}

func main() {}
