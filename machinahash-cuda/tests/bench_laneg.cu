// A lane G benchmark on the mainnet profile — gate 2 of the algorithm spec §7.
//
// Lane G has no scratchpad (ALGO §7.6). A thread needs only 65 words of shared memory, so the
// number of concurrent hashes is **not limited by VRAM** — the sweep below can keep widening the
// grid until the hashrate stops rising.
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <vector>

#include "../src/dag.cuh"
#include "../src/mh1.cuh"
#include "../src/mh1_fast.cuh"

#define CHECK(x)                                                                            \
    do {                                                                                    \
        cudaError_t e = (x);                                                                \
        if (e != cudaSuccess) {                                                             \
            fprintf(stderr, "CUDA %s:%d: %s\n", __FILE__, __LINE__, cudaGetErrorString(e)); \
            exit(2);                                                                        \
        }                                                                                   \
    } while (0)

extern __shared__ uint32_t smix[];

__global__ void k_bench(const uint8_t *header, uint64_t nonce_base, uint32_t per_thread,
                        const uint32_t *dag, MhFastMod items, uint8_t *sink) {
    uint32_t i = blockIdx.x * blockDim.x + threadIdx.x;
#if defined(MH_FAST)
    uint32_t *mix = smix + threadIdx.x * MHF_MIX_STRIDE;
#else
    uint32_t *mix = smix + threadIdx.x * MH_MIX_STRIDE;
#endif
    uint8_t h[32], acc = 0;
#if defined(MH_FAST)
    uint32_t hdr[20];
    for (int k = 0; k < 20; k++) hdr[k] = mh_get_u32le(header + k * 4);
#endif
    for (uint32_t j = 0; j < per_thread; j++) {
#if defined(MH_FAST)
        mhf::hash_lane_g(hdr, nonce_base + (uint64_t)i * per_thread + j, dag, items, mix, h);
#else
        mh_hash_lane_g(header, nonce_base + (uint64_t)i * per_thread + j, dag, items, mix, h);
#endif
        acc ^= h[0];  // so that the compiler does not optimize the hash away
    }
    sink[i] = acc;
}

static std::vector<uint32_t> load_u32(const char *path) {
    FILE *f = fopen(path, "rb");
    if (!f) { fprintf(stderr, "cannot open %s\n", path); exit(2); }
    fseek(f, 0, SEEK_END); long n = ftell(f); fseek(f, 0, SEEK_SET);
    std::vector<uint32_t> v(n / 4);
    if (fread(v.data(), 1, n, f) != (size_t)n) { fprintf(stderr, "short read\n"); exit(2); }
    fclose(f);
    return v;
}

int main(int argc, char **argv) {
    const char *dir = argc > 1 ? argv[1] : "fixtures-mainnet";
    size_t dag_gib = argc > 2 ? (size_t)atoll(argv[2]) : 1;

    cudaDeviceProp prop;
    CHECK(cudaGetDeviceProperties(&prop, 0));
    int mem_khz = 0, bus_bits = 0;
    CHECK(cudaDeviceGetAttribute(&mem_khz, cudaDevAttrMemoryClockRate, 0));
    CHECK(cudaDeviceGetAttribute(&bus_bits, cudaDevAttrGlobalMemoryBusWidth, 0));
    double peak_gbs = 2.0 * mem_khz * 1e3 * (bus_bits / 8.0) / 1e9;
    printf("# bench lane G — %s (sm_%d%d, %d SM, %.0f GB VRAM, ceiling %.0f GB/s)\n", prop.name,
           prop.major, prop.minor, prop.multiProcessorCount, prop.totalGlobalMem / 1e9, peak_gbs);

    char p[512];
    snprintf(p, sizeof p, "%s/cache.bin", dir);
    auto cache = load_u32(p);
    uint32_t cache_items = cache.size() / CACHE_ITEM_WORDS;
    size_t dag_bytes = dag_gib << 30;
    uint32_t items_n = (uint32_t)(dag_bytes / DAG_ITEM_BYTES);
    MhFastMod items = mh_fastmod_init(items_n);

    snprintf(p, sizeof p, "%s/randmath.txt", dir);
    FILE *rf = fopen(p, "r");
    if (!rf) { fprintf(stderr, "cannot open randmath.txt\n"); return 2; }
    MhMathOp ops[RANDMATH_OPS];
    for (int i = 0; i < RANDMATH_OPS; i++) {
        unsigned a, b, c;
        if (fscanf(rf, "%u %u %u", &a, &b, &c) != 3) return 2;
        ops[i] = {(uint8_t)a, (uint8_t)b, (uint8_t)c};
    }
    fclose(rf);
    CHECK(cudaMemcpyToSymbol(MH_OPS, ops, sizeof ops));

    uint32_t *d_cache, *d_dag;
    CHECK(cudaMalloc(&d_cache, cache.size() * 4));
    CHECK(cudaMalloc(&d_dag, dag_bytes));
    CHECK(cudaMemcpy(d_cache, cache.data(), cache.size() * 4, cudaMemcpyHostToDevice));
    k_dag_build<<<(items_n + 127) / 128, 128>>>(d_cache, cache_items, d_dag, items_n);
    CHECK(cudaDeviceSynchronize());
    CHECK(cudaGetLastError());
    CHECK(cudaFree(d_cache));
    printf("DAG %zu GiB built\n\n", dag_gib);

    uint8_t header[80];
    for (int i = 0; i < 80; i++) header[i] = (uint8_t)(i * 31 + 7);
    uint8_t *d_header;
    CHECK(cudaMalloc(&d_header, 80));
    CHECK(cudaMemcpy(d_header, header, 80, cudaMemcpyHostToDevice));

    // The only extra thing a thread needs is 65 words of shared memory. VRAM now only holds the DAG.
    printf("%8s %10s %12s %10s %8s\n", "threads", "total", "H/s", "GB/s DAG", "% ceil");
    double best = 0;
    size_t best_total = 0;
    int best_tpb = 0;
    const int TPB[] = {32, 64, 128};
    for (int ti = 0; ti < 3; ti++) {
        for (int bps = 1; bps <= 64; bps *= 2) {
            int tpb = TPB[ti];
            int blocks = prop.multiProcessorCount * bps;
            size_t total = (size_t)blocks * tpb;
#if defined(MH_FAST)
            size_t smem = (size_t)tpb * MHF_MIX_STRIDE * sizeof(uint32_t);
#else
            size_t smem = (size_t)tpb * MH_MIX_STRIDE * sizeof(uint32_t);
#endif
            if (smem > (size_t)prop.sharedMemPerBlock) continue;

            uint8_t *d_sink;
            CHECK(cudaMalloc(&d_sink, total));

            k_bench<<<blocks, tpb, smem>>>(d_header, 1, 1, d_dag, items, d_sink);
            CHECK(cudaDeviceSynchronize());
            CHECK(cudaGetLastError());

            cudaEvent_t t0, t1;
            CHECK(cudaEventCreate(&t0));
            CHECK(cudaEventCreate(&t1));
            uint32_t per_thread = 4;
            CHECK(cudaEventRecord(t0));
            k_bench<<<blocks, tpb, smem>>>(d_header, 1000, per_thread, d_dag, items, d_sink);
            CHECK(cudaEventRecord(t1));
            CHECK(cudaDeviceSynchronize());
            CHECK(cudaGetLastError());
            float ms = 0;
            CHECK(cudaEventElapsedTime(&ms, t0, t1));
            double hs = total * (double)per_thread / (ms / 1000.0);
            double gbs = hs * 1048576.0 / 1e9;
            printf("%8d %10zu %12.0f %10.1f %8.0f%s\n", tpb, total, hs, gbs,
                   100.0 * gbs / peak_gbs, hs > best ? "  <-" : "");
            if (hs > best) { best = hs; best_total = total; best_tpb = tpb; }

            CHECK(cudaFree(d_sink));
        }
    }

    printf("\nbest: %.0f H/s (%zu threads, %d per block)\n", best, best_total, best_tpb);
    printf("DAG reads: %.1f GB/s = %.0f %% of the card's theoretical ceiling\n", best * 1048576.0 / 1e9,
           100.0 * best * 1048576.0 / 1e9 / peak_gbs);
    return 0;
}
