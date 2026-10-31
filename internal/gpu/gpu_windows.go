//go:build windows

// Package gpu loads the miner's GPU plugin (powm-cuda.dll) at runtime through LoadLibrary.
//
// Why a plugin and not linking CUDA directly: a single miner binary has to run on a machine without an
// NVIDIA card too (CPU only), and building the miner must not require the CUDA toolkit. The plugin is
// built separately (nvcc drives MSVC there, see BUILDING.md) and sits next to the miner.
//
// This is the Windows version (LoadLibrary). It is the POSIX file with dlopen/dlsym swapped for
// LoadLibraryA/GetProcAddress; everything else is kept identical so the two stay comparable line by line.
package gpu

/*
#include <windows.h>
#include <stdint.h>
#include <stdlib.h>

typedef int (*f_abi)(void);
typedef int (*f_count)(void);
typedef int (*f_info)(int, char*, int, uint64_t*, int*, int*, int*, double*);
typedef void* (*f_create)(int, char*, int);
typedef void (*f_destroy)(void*);
typedef int (*f_epoch)(void*, const uint32_t*, uint32_t, uint32_t, const uint8_t*, char*, int);
typedef int (*f_job)(void*, const uint8_t*, const uint8_t*, char*, int);
typedef int (*f_search)(void*, uint64_t, uint64_t*, uint32_t, uint32_t*, uint64_t*, char*, int);
typedef int (*f_hash)(void*, const uint8_t*, uint64_t, uint8_t*, char*, int);
typedef void (*f_config)(void*, int*, int*, uint32_t*);

typedef struct {
	void *h;
	f_abi abi; f_count count; f_info info; f_create create; f_destroy destroy;
	f_epoch epoch; f_job job; f_search search; f_hash hash; f_config config;
} pmc_t;

// There is no dlerror() here: a failed LoadLibrary reports GetLastError() through *code.
static const char* pmc_load(pmc_t *p, const char *path, unsigned long *code) {
	*code = 0;
	p->h = (void*)LoadLibraryA(path);
	if (!p->h) { *code = GetLastError(); return "LoadLibrary failed"; }
	#define SYM(field, name) p->field = (void*)GetProcAddress((HMODULE)p->h, name); if (!p->field) return "missing symbol " name;
	SYM(abi, "pmc_abi") SYM(count, "pmc_device_count") SYM(info, "pmc_device_info")
	SYM(create, "pmc_create") SYM(destroy, "pmc_destroy") SYM(epoch, "pmc_set_epoch")
	SYM(job, "pmc_set_job") SYM(search, "pmc_search") SYM(hash, "pmc_hash") SYM(config, "pmc_config")
	return 0;
}
static int c_abi(pmc_t *p) { return p->abi(); }
static int c_count(pmc_t *p) { return p->count(); }
static int c_info(pmc_t *p, int d, char *n, int nc, uint64_t *m, int *a, int *b, int *s, double *pk) { return p->info(d, n, nc, m, a, b, s, pk); }
static void* c_create(pmc_t *p, int d, char *e, int ec) { return p->create(d, e, ec); }
static void c_destroy(pmc_t *p, void *c) { p->destroy(c); }
static int c_epoch(pmc_t *p, void *c, const uint32_t *cache, uint32_t w, uint32_t items, const uint8_t *ops, char *e, int ec) { return p->epoch(c, cache, w, items, ops, e, ec); }
static int c_job(pmc_t *p, void *c, const uint8_t *h, const uint8_t *t, char *e, int ec) { return p->job(c, h, t, e, ec); }
static int c_search(pmc_t *p, void *c, uint64_t s, uint64_t *o, uint32_t mo, uint32_t *f, uint64_t *hs, char *e, int ec) { return p->search(c, s, o, mo, f, hs, e, ec); }
static int c_hash(pmc_t *p, void *c, const uint8_t *h, uint64_t n, uint8_t *o, char *e, int ec) { return p->hash(c, h, n, o, e, ec); }
static void c_config(pmc_t *p, void *c, int *t, int *b, uint32_t *pt) { p->config(c, t, b, pt); }
*/
import "C"

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"unsafe"
)

// ABI is the version of the plugin interface the miner can speak.
const ABI = 1

// LibName is the plugin's file name.
const LibName = "powm-cuda.dll"

// Plugin is a loaded GPU plugin.
type Plugin struct {
	p C.pmc_t
}

// Load loads the plugin. An empty path means next to the miner binary, then the bare name, so that the
// normal DLL search path applies. The current working directory is deliberately not a candidate.
func Load(path string) (*Plugin, error) {
	candidates := []string{path}
	if path == "" {
		candidates = nil
		if exe, err := os.Executable(); err == nil {
			candidates = append(candidates, filepath.Join(filepath.Dir(exe), LibName))
		}
		candidates = append(candidates, LibName)
	}
	var lastErr error
	for _, c := range candidates {
		pl := &Plugin{}
		cs := C.CString(c)
		var code C.ulong
		msg := C.pmc_load(&pl.p, cs, &code)
		C.free(unsafe.Pointer(cs))
		if msg != nil {
			if code != 0 {
				lastErr = fmt.Errorf("%s: %s, error %d", c, C.GoString(msg), uint32(code))
			} else {
				lastErr = fmt.Errorf("%s: %s", c, C.GoString(msg))
			}
			continue
		}
		if v := int(C.c_abi(&pl.p)); v != ABI {
			return nil, fmt.Errorf("%s has interface v%d, the miner expects v%d — use the plugin from the same release", c, v, ABI)
		}
		return pl, nil
	}
	return nil, lastErr
}

// DeviceInfo describes a card.
type DeviceInfo struct {
	Index    int
	Name     string
	Memory   uint64
	SM       string // e.g. "8.6"
	SMCount  int
	PeakGBps float64
}

// Devices returns the cards the plugin can see.
func (p *Plugin) Devices() []DeviceInfo {
	n := int(C.c_count(&p.p))
	out := make([]DeviceInfo, 0, n)
	for i := 0; i < n; i++ {
		var name [128]C.char
		var mem C.uint64_t
		var maj, min, sms C.int
		var peak C.double
		if C.c_info(&p.p, C.int(i), &name[0], 128, &mem, &maj, &min, &sms, &peak) != 0 {
			continue
		}
		out = append(out, DeviceInfo{Index: i, Name: C.GoString(&name[0]), Memory: uint64(mem),
			SM: fmt.Sprintf("%d.%d", int(maj), int(min)), SMCount: int(sms), PeakGBps: float64(peak)})
	}
	return out
}

// Device is one card with its own context. The methods are not safe for concurrent use — one goroutine
// serves each card.
type Device struct {
	p     *Plugin
	ctx   unsafe.Pointer
	index int
}

// Open opens a card.
func (p *Plugin) Open(index int) (*Device, error) {
	var eb [256]C.char
	ctx := C.c_create(&p.p, C.int(index), &eb[0], 256)
	if ctx == nil {
		return nil, fmt.Errorf("GPU %d: %s", index, C.GoString(&eb[0]))
	}
	return &Device{p: p, ctx: ctx, index: index}, nil
}

// Close releases a card.
func (d *Device) Close() {
	if d.ctx != nil {
		C.c_destroy(&d.p.p, d.ctx)
		d.ctx = nil
	}
}

// Index is the plugin's device ordinal — the one nvidia-smi shows — not the position in the
// miner's selection.
func (d *Device) Index() int { return d.index }

// SetEpoch builds the epoch dataset on the card from the cache and tunes the grid.
func (d *Device) SetEpoch(cache []uint32, datasetItems uint32, ops [16][3]uint8) error {
	if len(cache) == 0 {
		return errors.New("gpu: an empty cache")
	}
	var flat [48]C.uint8_t
	for i, o := range ops {
		flat[i*3], flat[i*3+1], flat[i*3+2] = C.uint8_t(o[0]), C.uint8_t(o[1]), C.uint8_t(o[2])
	}
	var eb [256]C.char
	cp := (*C.uint32_t)(C.malloc(C.size_t(len(cache) * 4)))
	defer C.free(unsafe.Pointer(cp))
	copy(unsafe.Slice((*uint32)(unsafe.Pointer(cp)), len(cache)), cache)
	if C.c_epoch(&d.p.p, d.ctx, cp, C.uint32_t(len(cache)), C.uint32_t(datasetItems), &flat[0], &eb[0], 256) != 0 {
		return fmt.Errorf("gpu: %s", C.GoString(&eb[0]))
	}
	return nil
}

// SetJob sets the header (80 B without the nonce) and the target.
func (d *Device) SetJob(header *[80]byte, target *[32]byte) error {
	var eb [256]C.char
	h := (*C.uint8_t)(C.CBytes(header[:]))
	t := (*C.uint8_t)(C.CBytes(target[:]))
	defer C.free(unsafe.Pointer(h))
	defer C.free(unsafe.Pointer(t))
	if C.c_job(&d.p.p, d.ctx, h, t, &eb[0], 256) != 0 {
		return fmt.Errorf("gpu: %s", C.GoString(&eb[0]))
	}
	return nil
}

// Search scans one launch starting from `start`. It returns the nonces found and the hash count.
func (d *Device) Search(start uint64) ([]uint64, uint64, error) {
	var eb [256]C.char
	var out [64]C.uint64_t
	var found C.uint32_t
	var hashes C.uint64_t
	if C.c_search(&d.p.p, d.ctx, C.uint64_t(start), &out[0], 64, &found, &hashes, &eb[0], 256) != 0 {
		return nil, 0, fmt.Errorf("gpu: %s", C.GoString(&eb[0]))
	}
	nonces := make([]uint64, int(found))
	for i := range nonces {
		nonces[i] = uint64(out[i])
	}
	return nonces, uint64(hashes), nil
}

// Hash computes one lane G hash on the card (a bit-exactness check).
func (d *Device) Hash(header *[80]byte, nonce uint64) ([32]byte, error) {
	var eb [256]C.char
	var out [32]C.uint8_t
	h := (*C.uint8_t)(C.CBytes(header[:]))
	defer C.free(unsafe.Pointer(h))
	var res [32]byte
	if C.c_hash(&d.p.p, d.ctx, h, C.uint64_t(nonce), &out[0], &eb[0], 256) != 0 {
		return res, fmt.Errorf("gpu: %s", C.GoString(&eb[0]))
	}
	for i := range res {
		res[i] = byte(out[i])
	}
	return res, nil
}

// Config is the tuned grid.
func (d *Device) Config() (threadsPerBlock, blocks int, perThread uint32) {
	var t, b C.int
	var pt C.uint32_t
	C.c_config(&d.p.p, d.ctx, &t, &b, &pt)
	return int(t), int(b), uint32(pt)
}
