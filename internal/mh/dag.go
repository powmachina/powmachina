package mh

import (
	"encoding/binary"
	"runtime"
	"sync"
)

// The DAG for B-core — the epoch dataset and its cache.
//
// The model is Ethash-like: a small cache is derived deterministically from the epoch and any dataset
// item can be computed from the cache. A miner holds the whole dataset (bandwidth), a light client
// only the cache (computation). Both paths **must** give bit-identical results — otherwise the network
// falls apart into miners and light clients.

// DagParams are the sizes for a given network and epoch.
type DagParams struct {
	CacheBytes   int
	DatasetBytes int
	Epoch        uint64
}

// MainnetDag is the mainnet profile for a given epoch.
func MainnetDag(epoch uint64) DagParams {
	return DagParams{
		CacheBytes:   CacheBytesMainnet,
		DatasetBytes: dagBytesForEpoch(epoch),
		Epoch:        epoch,
	}
}

// TestDag is the small profile for tests and test vectors (a 1 MiB cache, an 8 MiB dataset).
func TestDag(epoch uint64) DagParams {
	return DagParams{CacheBytes: 1 << 20, DatasetBytes: 8 << 20, Epoch: epoch}
}

// CustomDag is an arbitrary size — for the benchmark, which measures the effect of the DAG's size.
func CustomDag(epoch uint64, cacheBytes, datasetBytes int) DagParams {
	return DagParams{CacheBytes: cacheBytes, DatasetBytes: datasetBytes, Epoch: epoch}
}

func (p DagParams) CacheItems() int   { return p.CacheBytes / CacheItemBytes }
func (p DagParams) DatasetItems() int { return p.DatasetBytes / DagItemBytes }

// DagAccess is access to the DAG's items. Both the full dataset (a miner) and the light
// client (computation) implement it.
type DagAccess interface {
	ItemCount() int
	ItemInto(idx int, out *[DagItemWords]uint32)
}

// ---------------------------------------------------------------- cache

// Cache is the epoch cache — 16 MiB on mainnet.
type Cache struct {
	Params DagParams
	words  []uint32
}

// BuildCache builds the cache for the given parameters.
func BuildCache(p DagParams) *Cache {
	n := p.CacheItems()
	c := &Cache{Params: p, words: make([]uint32, n*CacheItemWords)}

	x := newXof()
	var buf [CacheItemBytes]byte
	var epochLE [8]byte
	binary.LittleEndian.PutUint64(epochLE[:], p.Epoch)

	x.fill(buf[:], domainCache, epochLE[:])
	putWords(c.words[0:CacheItemWords], buf[:])

	var prev [CacheItemBytes]byte
	for i := 1; i < n; i++ {
		takeWords(prev[:], c.words[(i-1)*CacheItemWords:i*CacheItemWords])
		x.fill(buf[:], domainCache, prev[:])
		putWords(c.words[i*CacheItemWords:(i+1)*CacheItemWords], buf[:])
	}

	// the randmemohash passes — so the cache cannot be streamed from front to back
	var mixed [CacheItemBytes]byte
	for range CacheRounds {
		for i := range n {
			v := int(c.words[i*CacheItemWords]) % n
			prevIdx := (i + n - 1) % n
			for k := range CacheItemWords {
				xw := c.words[prevIdx*CacheItemWords+k] ^ c.words[v*CacheItemWords+k]
				binary.LittleEndian.PutUint32(mixed[k*4:], xw)
			}
			x.fill(buf[:], domainCache, mixed[:])
			putWords(c.words[i*CacheItemWords:(i+1)*CacheItemWords], buf[:])
		}
	}
	return c
}

// Words are the cache's raw words — for a GPU that generates the dataset itself (F3).
func (c *Cache) Words() []uint32 { return c.words }

func (c *Cache) item(i int) []uint32 {
	o := (i % c.Params.CacheItems()) * CacheItemWords
	return c.words[o : o+CacheItemWords]
}

// datasetItem computes one dataset item from the cache — this is the cost of a light verify.
// `s` is the caller's workspace, so nothing is allocated per item.
func (c *Cache) datasetItem(s *itemState, idx int, out *[DagItemWords]uint32) {
	n := c.Params.CacheItems()

	takeWords(s.parent0[:], c.item(idx))
	binary.LittleEndian.PutUint64(s.idxLE[:], uint64(idx))
	s.x.fill(s.buf[:], domainItem, s.idxLE[:], s.parent0[:])
	for i := range out {
		out[i] = binary.LittleEndian.Uint32(s.buf[i*4:])
	}

	for j := range DagParents {
		p := int(fnv1a(uint32(idx)^uint32(j), out[j%DagItemWords])) % n
		parent := c.item(p)
		base := (j % 4) * CacheItemWords
		for k := range CacheItemWords {
			out[base+k] = fnv1a(out[base+k], parent[k])
		}
	}

	for i, w := range out {
		binary.LittleEndian.PutUint32(s.buf[i*4:], w)
	}
	s.x.fill(s.buf[:], domainItem, domainItemFin, s.buf[:])
	for i := range out {
		out[i] = binary.LittleEndian.Uint32(s.buf[i*4:])
	}
}

// itemState is the workspace for computing dataset items.
type itemState struct {
	x       *xof
	buf     [DagItemBytes]byte
	parent0 [CacheItemBytes]byte
	idxLE   [8]byte
}

func newItemState() *itemState { return &itemState{x: newXof()} }

var domainItemFin = []byte("fin")

// ---------------------------------------------------------------- light

// LightDag computes items from the cache. Slow, but without 1 GiB of RAM.
//
// It is not safe for concurrent use — it holds a workspace. Every thread should have its own.
type LightDag struct {
	cache *Cache
	st    *itemState
}

func NewLightDag(p DagParams) *LightDag { return NewLightDagFrom(BuildCache(p)) }

// NewLightDagFrom shares an already built cache. Once built, the cache is read-only, so any number of
// LightDags may share it.
func NewLightDagFrom(c *Cache) *LightDag { return &LightDag{cache: c, st: newItemState()} }

func (d *LightDag) Cache() *Cache { return d.cache }
func (d *LightDag) ItemCount() int {
	return d.cache.Params.DatasetItems()
}
func (d *LightDag) ItemInto(idx int, out *[DagItemWords]uint32) {
	d.cache.datasetItem(d.st, idx, out)
}

// ---------------------------------------------------------------- full

// FullDag is the full dataset in memory — this is what a miner uses. It is safe for concurrent reads.
type FullDag struct {
	Params DagParams
	words  []uint32
}

// BuildFullDag builds both the cache and the dataset.
func BuildFullDag(p DagParams) *FullDag { return BuildFullDagFrom(BuildCache(p)) }

// BuildFullDagFrom generates the dataset from a finished cache. It runs on every core — with 1 GiB
// that is 4 194 304 items and serially it would take minutes.
func BuildFullDagFrom(c *Cache) *FullDag {
	p := c.Params
	n := p.DatasetItems()
	d := &FullDag{Params: p, words: make([]uint32, n*DagItemWords)}

	workers := min(runtime.NumCPU(), n)
	if workers < 1 {
		workers = 1
	}
	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			st := newItemState()
			var item [DagItemWords]uint32
			for i := w; i < n; i += workers {
				c.datasetItem(st, i, &item)
				copy(d.words[i*DagItemWords:(i+1)*DagItemWords], item[:])
			}
		}(w)
	}
	wg.Wait()
	return d
}

// Bytes is the dataset's size in bytes.
func (d *FullDag) Bytes() int { return len(d.words) * 4 }

// Words are the dataset's raw words — for comparison against a GPU-generated dataset (F3).
func (d *FullDag) Words() []uint32 { return d.words }

func (d *FullDag) ItemCount() int { return d.Params.DatasetItems() }

func (d *FullDag) ItemInto(idx int, out *[DagItemWords]uint32) {
	o := idx * DagItemWords
	copy(out[:], d.words[o:o+DagItemWords])
}

// ---------------------------------------------------------------- helpers

// putWords writes little-endian bytes into words.
func putWords(dst []uint32, src []byte) {
	for i := range dst {
		dst[i] = binary.LittleEndian.Uint32(src[i*4:])
	}
}

// takeWords writes words out as little-endian bytes.
func takeWords(dst []byte, src []uint32) {
	for i, w := range src {
		binary.LittleEndian.PutUint32(dst[i*4:], w)
	}
}
