// Package randomx is a thin, safe wrapper over the vendored RandomX v2 —
// the CPU half of MachinaHash (lane C, "MachinaHash-C").
//
// There is no algorithm logic here and none belongs here: RandomX is **vendored,
// not rewritten** (csrc/VENDOR-RANDOMX.md). The only change from upstream is the
// Argon2d salt, which separates our network from Monero — without it, rented
// hashrate could be pointed at PowMachina.
//
// # Why RandomX
//
// A memory-hard core of our own does not pass gate 1 (the algorithm spec §7.5): a 96 GB
// card computes it faster than a desktop Ryzen. The only lever that really works
// against a GPU is data-dependent branching — and that means a virtual machine.
//
// # Security
//
// RandomX generates and executes machine code (JIT). The node runs it over block data
// from strangers, so the JIT is enabled **always together with RANDOMX_FLAG_SECURE** (W^X:
// a page is never writable and executable at the same time). Enabling the JIT without
// SECURE is not possible through this API — for the Verifier role there is no path to it.
package randomx

/*
// The architecture-dependent switches live in flags_amd64.go / flags_arm64.go — `-maes` does not
// exist on arm64 and the compiler would fail on it.
#cgo CFLAGS:   -O3 -w -I${SRCDIR}/csrc/randomx/src -include ${SRCDIR}/mh_salt.h
#cgo CXXFLAGS: -O3 -w -std=c++11 -I${SRCDIR}/csrc/randomx/src -include ${SRCDIR}/mh_salt.h
#cgo LDFLAGS:  -lstdc++

#include <stdlib.h>
#include "randomx.h"
*/
import "C"

import (
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"unsafe"

	// The Argon2 variants live in their own packages because of the compiler switches.
	// These are blank imports: RandomX references them and the linker needs them.
	_ "powmachina/internal/randomx/argon2avx2"
	_ "powmachina/internal/randomx/argon2ssse3"
)

// The RandomX flags. These are the values from randomx.h; they are duplicated here only so they can
// be used from pure Go code (e.g. in FlagsString).
const (
	flagLargePages  = C.RANDOMX_FLAG_LARGE_PAGES
	flagHardAES     = C.RANDOMX_FLAG_HARD_AES
	flagFullMem     = C.RANDOMX_FLAG_FULL_MEM
	flagJIT         = C.RANDOMX_FLAG_JIT
	flagSecure      = C.RANDOMX_FLAG_SECURE
	flagArgon2SSSE3 = C.RANDOMX_FLAG_ARGON2_SSSE3
	flagArgon2AVX2  = C.RANDOMX_FLAG_ARGON2_AVX2
	flagV2          = C.RANDOMX_FLAG_V2
)

// Mode says how much memory is traded for speed.
type Mode int

const (
	// Light uses the cache only (~256 MiB) and is orders of magnitude slower. It is enough for a node:
	// it verifies one hash per block.
	Light Mode = iota
	// Fast holds the whole dataset (~2.1 GiB). This is what the miner runs.
	Fast
)

func (m Mode) String() string {
	if m == Fast {
		return "fast"
	}
	return "light"
}

// Role says whose data is being hashed — that decides whether W^X is paid for.
//
// SECURE forces the JIT to flip page permissions (mprotect) for each of the eight programs
// per hash. On a machine with dozens of threads that means a TLB shootdown across the cores,
// so it costs performance (2.17x on 96 threads). The dividing line is whose data is hashed:
//
//   - A node verifies headers sent to it by anyone → Verifier, SECURE always.
//     It pays for one hash per block, so the cost is irrelevant.
//   - A miner hashes its own template → Miner, SECURE can be turned off. It protects against
//     exploitation of a possible JIT bug, not against bad input; the miner makes its own.
type Role int

const (
	// Verifier processes foreign data. W^X always.
	Verifier Role = iota
	// MinerSecure hashes its own data but keeps W^X.
	MinerSecure
	// MinerFast hashes its own data without W^X.
	MinerFast
)

func (r Role) wantsSecure() bool { return r != MinerFast }

// ErrAlloc means RandomX did not get memory. In Fast mode this is usually a shortage of
// RAM (~2.1 GiB), in Light ~256 MiB.
var ErrAlloc = errors.New("randomx: allocation failed")

func flagsFor(role Role, largePages bool) C.randomx_flags {
	// randomx_get_flags() detects what the CPU can do (HARD_AES, ARGON2_*, JIT). On top of that
	// we add V2 — RandomX v2 is our consensus, not a choice.
	f := C.randomx_get_flags() | flagV2
	if f&flagJIT != 0 && role.wantsSecure() {
		f |= flagSecure
	} else {
		f &^= flagSecure
	}
	if largePages {
		f |= flagLargePages
	}
	return f
}

func baseFlags() C.randomx_flags { return flagsFor(Verifier, false) }

// HasHardwareAES says whether the CPU has hardware AES. Without it lane C is several times slower.
func HasHardwareAES() bool { return C.randomx_get_flags()&flagHardAES != 0 }

// JITEnabled says whether the JIT runs at all. Without it RandomX goes through the interpreter.
func JITEnabled() bool { return baseFlags()&flagJIT != 0 }

// JITIsSecure says whether the JIT runs with W^X enforced. A node should not process foreign
// blocks without it.
//
// It returns true even when the JIT does not run at all — the interpreter is safe in itself, only
// slow. JITEnabled is there to tell the two apart.
func JITIsSecure() bool {
	f := baseFlags()
	return f&flagJIT == 0 || f&flagSecure != 0
}

// Flags are the raw flags RandomX runs with — for diagnostics.
func Flags() uint32 { return uint32(baseFlags()) }

// FlagsString is a breakdown of the flags for the log.
func FlagsString() string { return describeFlags(baseFlags()) }

func describeFlags(f C.randomx_flags) string {
	var on []string
	for _, e := range []struct {
		bit  C.randomx_flags
		name string
	}{
		{flagLargePages, "LARGE_PAGES"},
		{flagHardAES, "HARD_AES"},
		{flagFullMem, "FULL_MEM"},
		{flagJIT, "JIT"},
		{flagSecure, "SECURE"},
		{flagArgon2SSSE3, "ARGON2_SSSE3"},
		{flagArgon2AVX2, "ARGON2_AVX2"},
		{flagV2, "V2"},
	} {
		if f&e.bit != 0 {
			on = append(on, e.name)
		}
	}
	return fmt.Sprintf("%d [%s]", uint32(f), strings.Join(on, "|"))
}

// KeyContext is the cache (and in Fast mode the dataset too) for one key. It is built once per
// epoch, not per hash, and is safe for concurrent use — after initialization it is read-only.
// The VMs themselves are per-thread (see VM).
type KeyContext struct {
	cache   *C.randomx_cache
	dataset *C.randomx_dataset
	mode    Mode
	flags   C.randomx_flags

	mu     sync.Mutex
	closed bool
	vms    int // how many live VMs hold it
}

// NewKeyContext builds a context for the given key in the Verifier role with large pages.
func NewKeyContext(key []byte, mode Mode) (*KeyContext, error) {
	return NewKeyContextAs(key, mode, Verifier, true)
}

// NewKeyContextAs is the full version. largePages is only a request: when the system does not have
// them (vm.nr_hugepages), the allocation is retried without them. Running silently slower is better
// than refusing to start — the caller can ask through UsesLargePages.
func NewKeyContextAs(key []byte, mode Mode, role Role, largePages bool) (*KeyContext, error) {
	if len(key) == 0 {
		return nil, errors.New("randomx: the key must not be empty")
	}
	flags := flagsFor(role, largePages)
	if mode == Fast {
		flags |= flagFullMem
	}

	cache := C.randomx_alloc_cache(flags)
	if cache == nil && largePages {
		flags &^= flagLargePages
		cache = C.randomx_alloc_cache(flags)
	}
	if cache == nil {
		return nil, fmt.Errorf("%w: cache", ErrAlloc)
	}
	C.randomx_init_cache(cache, unsafe.Pointer(&key[0]), C.size_t(len(key)))

	var dataset *C.randomx_dataset
	if mode == Fast {
		dataset = C.randomx_alloc_dataset(flags)
		if dataset == nil && flags&flagLargePages != 0 {
			flags &^= flagLargePages
			dataset = C.randomx_alloc_dataset(flags)
		}
		if dataset == nil {
			C.randomx_release_cache(cache)
			return nil, fmt.Errorf("%w: dataset (~2.1 GiB)", ErrAlloc)
		}
		initDataset(dataset, cache)
	}

	ctx := &KeyContext{cache: cache, dataset: dataset, mode: mode, flags: flags}
	runtime.SetFinalizer(ctx, (*KeyContext).Close)
	return ctx, nil
}

// initDataset fills the dataset in parallel. randomx_init_dataset may only be called concurrently
// on disjoint ranges — this split takes care of that.
func initDataset(dataset *C.randomx_dataset, cache *C.randomx_cache) {
	count := C.randomx_dataset_item_count()
	threads := runtime.NumCPU()
	if uint64(threads) > uint64(count) {
		threads = int(count)
	}
	if threads < 1 {
		threads = 1
	}
	per := count / C.ulong(threads)

	var wg sync.WaitGroup
	for t := range threads {
		start := per * C.ulong(t)
		n := per
		if t+1 == threads {
			n = count - start
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			C.randomx_init_dataset(dataset, cache, start, n)
		}()
	}
	wg.Wait()
}

// Mode returns the mode the context runs in.
func (c *KeyContext) Mode() Mode { return c.mode }

// UsesLargePages says whether the allocation in large pages succeeded.
func (c *KeyContext) UsesLargePages() bool { return c.flags&flagLargePages != 0 }

// Flags are the flags this context actually runs with (after any fallback).
func (c *KeyContext) Flags() uint32 { return uint32(c.flags) }

// FlagsString is a breakdown of this context's flags.
func (c *KeyContext) FlagsString() string { return describeFlags(c.flags) }

// NewVM creates a virtual machine for one thread. A VM keeps the context alive — the context
// must not be released before all of its VMs.
func (c *KeyContext) NewVM() (*VM, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, errors.New("randomx: the context is already closed")
	}
	var cacheArg *C.randomx_cache
	if c.mode == Light {
		cacheArg = c.cache
	}
	p := C.randomx_create_vm(c.flags, cacheArg, c.dataset)
	if p == nil {
		return nil, fmt.Errorf("%w: vm", ErrAlloc)
	}
	c.vms++
	vm := &VM{vm: p, ctx: c}
	runtime.SetFinalizer(vm, (*VM).Close)
	return vm, nil
}

// Close releases both the cache and the dataset. It is idempotent. It may only be called once all
// VMs from this context are closed; otherwise Close waits until the last VM leaves.
func (c *KeyContext) Close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	free := c.vms == 0
	c.mu.Unlock()

	runtime.SetFinalizer(c, nil)
	if free {
		c.release()
	}
}

func (c *KeyContext) release() {
	if c.dataset != nil {
		C.randomx_release_dataset(c.dataset)
		c.dataset = nil
	}
	if c.cache != nil {
		C.randomx_release_cache(c.cache)
		c.cache = nil
	}
}

func (c *KeyContext) vmClosed() {
	c.mu.Lock()
	c.vms--
	free := c.closed && c.vms == 0
	c.mu.Unlock()
	if free {
		c.release()
	}
}

// VM is a virtual machine for **one thread**. It has internal state, so calling Hash concurrently
// from several goroutines is a caller error — every thread should have its own.
type VM struct {
	vm  *C.randomx_vm
	ctx *KeyContext
}

// Hash computes the RandomX hash of the input. The output is the lane C PoW hash directly — no wrapper.
func (v *VM) Hash(input []byte) [32]byte {
	var out [32]byte
	v.HashInto(input, &out)
	return out
}

// HashInto is Hash without allocating the output — for the miner loop.
func (v *VM) HashInto(input []byte, out *[32]byte) {
	// An empty input would give a nil pointer; the PoW preimage is always 89 B, but the API
	// should not crash on whatever a test calls it with.
	var empty [1]byte
	p := unsafe.Pointer(&empty[0])
	if len(input) > 0 {
		p = unsafe.Pointer(&input[0])
	}
	C.randomx_calculate_hash(v.vm, p, C.size_t(len(input)), unsafe.Pointer(&out[0]))
	runtime.KeepAlive(input)
}

// HashFirst starts chained hashing (miner): while the hash of `input` is being computed, the next
// call to HashNext already prepares the program for the following input. RandomX can overlap this,
// because the program of the next hash depends only on the input, not on the previous result.
func (v *VM) HashFirst(input []byte) {
	C.randomx_calculate_hash_first(v.vm, unsafe.Pointer(&input[0]), C.size_t(len(input)))
	runtime.KeepAlive(input)
}

// HashNext returns the hash of the input from the previous call and starts `next`.
func (v *VM) HashNext(next []byte, out *[32]byte) {
	C.randomx_calculate_hash_next(v.vm, unsafe.Pointer(&next[0]), C.size_t(len(next)), unsafe.Pointer(&out[0]))
	runtime.KeepAlive(next)
}

// Close releases the VM. It is idempotent.
func (v *VM) Close() {
	if v.vm == nil {
		return
	}
	C.randomx_destroy_vm(v.vm)
	v.vm = nil
	runtime.SetFinalizer(v, nil)
	v.ctx.vmClosed()
}
