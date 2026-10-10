// libpowm-cuda — the implementation. The kernel is the same mh1.cuh that passed 500/500 vectors
// bit-exact (tests/test_lanes.cu); this is only the wrapper: the DAG on the card, the search and the
// grid tuning.
#include <cstdio>
#include <cstring>

#include "../src/dag.cuh"
#include "../src/mh1.cuh"
#include "../src/mh1_fast.cuh"
#include "powm_cuda.h"

extern __shared__ uint32_t pmc_smix[];

#define PMC_MAX_HITS 64

// The hash is less than or equal to the target (both big-endian) — a mirror of mh.MeetsTarget.
__device__ __forceinline__ bool pmc_meets(const uint8_t h[32], const uint8_t *target) {
#pragma unroll
    for (int i = 0; i < 32; i++) {
        if (h[i] < target[i]) return true;
        if (h[i] > target[i]) return false;
    }
    return true;
}

__global__ void k_search(const uint8_t *header, const uint8_t *target, uint64_t start,
                         uint32_t per_thread, const uint32_t *dag, MhFastMod items,
                         uint64_t *hits, uint32_t *count) {
    __shared__ uint32_t sh_header[20];
    __shared__ uint8_t sh_target[32];
    if (threadIdx.x < 20) sh_header[threadIdx.x] = mh_get_u32le(header + threadIdx.x * 4);
    if (threadIdx.x < 32) sh_target[threadIdx.x] = target[threadIdx.x];
    __syncthreads();
    uint32_t hdr[20];
#pragma unroll
    for (int i = 0; i < 20; i++) hdr[i] = sh_header[i];
    uint64_t tid = (uint64_t)blockIdx.x * blockDim.x + threadIdx.x;
    uint64_t total = (uint64_t)gridDim.x * blockDim.x;
    uint32_t *mix = pmc_smix + threadIdx.x * MHF_MIX_STRIDE;
    uint8_t h[32];
    for (uint32_t j = 0; j < per_thread; j++) {
        // interleaved: adjacent threads take adjacent nonces (the result does not matter, only the coverage)
        uint64_t nonce = start + tid + (uint64_t)j * total;
        mhf::hash_lane_g(hdr, nonce, dag, items, mix, h);
        if (pmc_meets(h, sh_target)) {
            uint32_t k = atomicAdd(count, 1u);
            if (k < PMC_MAX_HITS) hits[k] = nonce;
        }
    }
}

// The verification hash: the fast path needs a whole warp, so 32 threads compute the same nonce
// and thread 0 writes the result.
__global__ void k_one(const uint8_t *header, uint64_t nonce, const uint32_t *dag, MhFastMod items,
                      uint8_t *out) {
    uint32_t *mix = pmc_smix + threadIdx.x * MHF_MIX_STRIDE;
    uint32_t hdr[20];
    for (int i = 0; i < 20; i++) hdr[i] = mh_get_u32le(header + i * 4);
    uint8_t h[32];
    mhf::hash_lane_g(hdr, nonce, dag, items, mix, h);
    if (threadIdx.x == 0)
        for (int i = 0; i < 32; i++) out[i] = h[i];
}

struct Ctx {
    int dev;
    cudaStream_t stream;
    uint32_t *dag;
    size_t dag_bytes;
    MhFastMod items;
    uint8_t *header, *target, *out32;
    uint64_t *hits;
    uint32_t *count;
    int tpb, blocks;
    uint32_t per_thread;
    bool epoch_ready, job_ready;
};

static int fail(char *err, int cap, const char *what, cudaError_t e) {
    if (err && cap > 0) snprintf(err, cap, "%s: %s", what, cudaGetErrorString(e));
    return (int)e == 0 ? 1 : (int)e;
}
#define TRY(x, what)                                    \
    do {                                                \
        cudaError_t e_ = (x);                           \
        if (e_ != cudaSuccess) return fail(err, err_cap, what, e_); \
    } while (0)

extern "C" PMC_API int pmc_abi(void) { return PMC_ABI; }

extern "C" PMC_API int pmc_device_count(void) {
    int n = 0;
    if (cudaGetDeviceCount(&n) != cudaSuccess) return 0;
    return n;
}

extern "C" PMC_API int pmc_device_info(int dev, char *name, int name_cap, uint64_t *mem, int *maj, int *min,
                               int *sms, double *peak) {
    cudaDeviceProp p;
    if (cudaGetDeviceProperties(&p, dev) != cudaSuccess) return 1;
    int khz = 0, bits = 0;
    cudaDeviceGetAttribute(&khz, cudaDevAttrMemoryClockRate, dev);
    cudaDeviceGetAttribute(&bits, cudaDevAttrGlobalMemoryBusWidth, dev);
    if (name && name_cap > 0) snprintf(name, name_cap, "%s", p.name);
    *mem = p.totalGlobalMem;
    *maj = p.major;
    *min = p.minor;
    *sms = p.multiProcessorCount;
    *peak = 2.0 * khz * 1e3 * (bits / 8.0) / 1e9;
    return 0;
}

extern "C" PMC_API void *pmc_create(int dev, char *err, int err_cap) {
    Ctx *c = new Ctx();
    memset(c, 0, sizeof *c);
    c->dev = dev;
    cudaError_t e = cudaSetDevice(dev);
    if (e == cudaSuccess) e = cudaStreamCreateWithFlags(&c->stream, cudaStreamNonBlocking);
    if (e == cudaSuccess) e = cudaMalloc(&c->header, 80);
    if (e == cudaSuccess) e = cudaMalloc(&c->target, 32);
    if (e == cudaSuccess) e = cudaMalloc(&c->out32, 32);
    if (e == cudaSuccess) e = cudaMalloc(&c->hits, PMC_MAX_HITS * sizeof(uint64_t));
    if (e == cudaSuccess) e = cudaMalloc(&c->count, sizeof(uint32_t));
    if (e != cudaSuccess) {
        fail(err, err_cap, "pmc_create", e);
        delete c;
        return nullptr;
    }
    return c;
}

extern "C" PMC_API void pmc_destroy(void *p) {
    Ctx *c = (Ctx *)p;
    if (!c) return;
    cudaSetDevice(c->dev);
    cudaFree(c->dag);
    cudaFree(c->header);
    cudaFree(c->target);
    cudaFree(c->out32);
    cudaFree(c->hits);
    cudaFree(c->count);
    cudaStreamDestroy(c->stream);
    delete c;
}

static size_t smem_for(int tpb) { return (size_t)tpb * MHF_MIX_STRIDE * sizeof(uint32_t); }

// One grid measurement: it returns H/s.
static double measure(Ctx *c, int tpb, int blocks, uint32_t per_thread) {
    cudaEvent_t t0, t1;
    cudaEventCreate(&t0);
    cudaEventCreate(&t1);
    cudaMemsetAsync(c->count, 0, 4, c->stream);
    cudaEventRecord(t0, c->stream);
    k_search<<<blocks, tpb, smem_for(tpb), c->stream>>>(c->header, c->target, 1ull << 40,
                                                        per_thread, c->dag, c->items, c->hits,
                                                        c->count);
    cudaEventRecord(t1, c->stream);
    cudaEventSynchronize(t1);
    float ms = 0;
    cudaEventElapsedTime(&ms, t0, t1);
    cudaEventDestroy(t0);
    cudaEventDestroy(t1);
    if (cudaGetLastError() != cudaSuccess || ms <= 0) return 0;
    return (double)blocks * tpb * per_thread / (ms / 1000.0);
}

// Tuning: threads per block x blocks per SM, with a zero target (nothing is ever found). Each
// measurement has ~20 ms of work (per_thread from an estimate), so the whole tuning takes under a
// second even on the biggest card. Then per_thread is picked so that one launch takes ~50 ms — enough
// for negligible overhead, little enough not to switch slowly after a new block.
static void autotune(Ctx *c) {
    cudaDeviceProp p;
    cudaGetDeviceProperties(&p, c->dev);
    uint8_t zero[32] = {0}, hdr[80] = {0};
    cudaMemcpy(c->target, zero, 32, cudaMemcpyHostToDevice);
    cudaMemcpy(c->header, hdr, 80, cudaMemcpyHostToDevice);
    int sm = p.multiProcessorCount;
    measure(c, 64, sm * 4, 1);  // warm-up
    double est = measure(c, 64, sm * 4, 1);
    if (est <= 0) est = 1e5;
    const int TPB[] = {32, 64, 128, 256};
    const int BPS[] = {1, 2, 4, 8, 16, 32};
    double best = 0;
    c->tpb = 64;
    c->blocks = sm * 4;
    for (int tpb : TPB) {
        if (smem_for(tpb) > (size_t)p.sharedMemPerBlock) continue;
        for (int bps : BPS) {
            int blocks = sm * bps;
            double threads = (double)blocks * tpb;
            uint32_t pt = (uint32_t)(est * 0.02 / threads);
            if (pt < 1) pt = 1;
            double hs = measure(c, tpb, blocks, pt);
            if (hs > best * 1.01) {
                best = hs;
                c->tpb = tpb;
                c->blocks = blocks;
            }
        }
    }
    uint32_t pt = (uint32_t)(best * 0.05 / ((double)c->tpb * c->blocks));
    c->per_thread = pt < 1 ? 1 : pt;
}

extern "C" PMC_API int pmc_set_epoch(void *p, const uint32_t *cache, uint32_t cache_words,
                             uint32_t dataset_items, const uint8_t *ops, char *err, int err_cap) {
    Ctx *c = (Ctx *)p;
    TRY(cudaSetDevice(c->dev), "cudaSetDevice");
    c->epoch_ready = false;
    size_t need = (size_t)dataset_items * DAG_ITEM_BYTES;
    if (need != c->dag_bytes) {
        if (c->dag) cudaFree(c->dag);
        c->dag = nullptr;
        c->dag_bytes = 0;
        TRY(cudaMalloc(&c->dag, need), "the DAG does not fit in VRAM");
        c->dag_bytes = need;
    }
    MhMathOp mops[RANDMATH_OPS];
    for (int i = 0; i < RANDMATH_OPS; i++) mops[i] = {ops[i * 3], ops[i * 3 + 1], ops[i * 3 + 2]};
    TRY(cudaMemcpyToSymbol(MH_OPS, mops, sizeof mops), "RANDMATH");
    uint32_t *d_cache = nullptr;
    TRY(cudaMalloc(&d_cache, (size_t)cache_words * 4), "cache");
    cudaError_t e = cudaMemcpy(d_cache, cache, (size_t)cache_words * 4, cudaMemcpyHostToDevice);
    if (e == cudaSuccess) {
        k_dag_build<<<(dataset_items + 127) / 128, 128>>>(d_cache, cache_words / CACHE_ITEM_WORDS,
                                                          c->dag, dataset_items);
        e = cudaDeviceSynchronize();
        if (e == cudaSuccess) e = cudaGetLastError();
    }
    cudaFree(d_cache);
    if (e != cudaSuccess) return fail(err, err_cap, "building the DAG", e);
    c->items = mh_fastmod_init(dataset_items);
    autotune(c);
    c->epoch_ready = true;
    c->job_ready = false;
    return 0;
}

extern "C" PMC_API int pmc_set_job(void *p, const uint8_t *header80, const uint8_t *target32, char *err,
                           int err_cap) {
    Ctx *c = (Ctx *)p;
    TRY(cudaSetDevice(c->dev), "cudaSetDevice");
    TRY(cudaMemcpyAsync(c->header, header80, 80, cudaMemcpyHostToDevice, c->stream), "header");
    TRY(cudaMemcpyAsync(c->target, target32, 32, cudaMemcpyHostToDevice, c->stream), "target");
    TRY(cudaStreamSynchronize(c->stream), "sync");
    c->job_ready = true;
    return 0;
}

extern "C" PMC_API int pmc_search(void *p, uint64_t start, uint64_t *out, uint32_t max_out,
                          uint32_t *found, uint64_t *hashes, char *err, int err_cap) {
    Ctx *c = (Ctx *)p;
    *found = 0;
    *hashes = 0;
    if (!c->epoch_ready || !c->job_ready) {
        if (err && err_cap > 0) snprintf(err, err_cap, "the epoch or the work is not set");
        return 1;
    }
    TRY(cudaSetDevice(c->dev), "cudaSetDevice");
    TRY(cudaMemsetAsync(c->count, 0, 4, c->stream), "memset");
    k_search<<<c->blocks, c->tpb, smem_for(c->tpb), c->stream>>>(
        c->header, c->target, start, c->per_thread, c->dag, c->items, c->hits, c->count);
    TRY(cudaGetLastError(), "k_search");
    uint32_t n = 0;
    TRY(cudaMemcpyAsync(&n, c->count, 4, cudaMemcpyDeviceToHost, c->stream), "count");
    TRY(cudaStreamSynchronize(c->stream), "sync");
    if (n > PMC_MAX_HITS) n = PMC_MAX_HITS;
    if (n > max_out) n = max_out;
    if (n > 0) {
        TRY(cudaMemcpy(out, c->hits, n * sizeof(uint64_t), cudaMemcpyDeviceToHost), "hits");
    }
    *found = n;
    *hashes = (uint64_t)c->blocks * c->tpb * c->per_thread;
    return 0;
}

extern "C" PMC_API int pmc_hash(void *p, const uint8_t *header80, uint64_t nonce, uint8_t *out32,
                        char *err, int err_cap) {
    Ctx *c = (Ctx *)p;
    TRY(cudaSetDevice(c->dev), "cudaSetDevice");
    uint8_t *d_h = nullptr;
    TRY(cudaMalloc(&d_h, 80), "malloc");
    cudaError_t e = cudaMemcpy(d_h, header80, 80, cudaMemcpyHostToDevice);
    if (e == cudaSuccess) {
        k_one<<<1, 32, smem_for(32)>>>(d_h, nonce, c->dag, c->items, c->out32);
        e = cudaDeviceSynchronize();
    }
    if (e == cudaSuccess) e = cudaMemcpy(out32, c->out32, 32, cudaMemcpyDeviceToHost);
    cudaFree(d_h);
    if (e != cudaSuccess) return fail(err, err_cap, "pmc_hash", e);
    return 0;
}

extern "C" PMC_API void pmc_config(void *p, int *tpb, int *blocks, uint32_t *per_thread) {
    Ctx *c = (Ctx *)p;
    *tpb = c->tpb;
    *blocks = c->blocks;
    *per_thread = c->per_thread;
}
