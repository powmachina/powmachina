// A mainnet-sized DAG: it builds a 1 GiB dataset on the GPU from a 16 MiB cache, times it
// and verifies spot checks against internal/mh.
//
// Why spot checks and not the whole dataset: 1 GiB of reference data would have to be transferred
// from the CPU. The indexes are scattered across the whole range including 0, n/2 and n-1 — that is,
// exactly where an overflowed index or a wrong offset would show up.

#include <cstdlib>
#include <cstring>
#include <string>
#include <vector>

#include "../src/dag.cuh"

#define CHECK(x)                                                                            \
    do {                                                                                    \
        cudaError_t e = (x);                                                                \
        if (e != cudaSuccess) {                                                             \
            fprintf(stderr, "CUDA %s:%d: %s\n", __FILE__, __LINE__, cudaGetErrorString(e)); \
            exit(2);                                                                        \
        }                                                                                   \
    } while (0)

static std::vector<uint32_t> load(const char *path) {
    FILE *f = fopen(path, "rb");
    if (!f) { fprintf(stderr, "cannot open %s\n", path); exit(2); }
    fseek(f, 0, SEEK_END);
    long n = ftell(f);
    fseek(f, 0, SEEK_SET);
    std::vector<uint32_t> v(n / 4);
    if (fread(v.data(), 1, n, f) != (size_t)n) { fprintf(stderr, "short read\n"); exit(2); }
    fclose(f);
    return v;
}

int main(int argc, char **argv) {
    const char *dir = argc > 1 ? argv[1] : "fixtures-mainnet";
    size_t dataset_bytes = (argc > 2 ? (size_t)atoll(argv[2]) : 1) << 30;

    char p[512];
    snprintf(p, sizeof p, "%s/cache.bin", dir);
    auto cache = load(p);
    uint32_t cache_items = cache.size() / CACHE_ITEM_WORDS;
    uint32_t dataset_items = (uint32_t)(dataset_bytes / DAG_ITEM_BYTES);

    size_t free_b = 0, total_b = 0;
    CHECK(cudaMemGetInfo(&free_b, &total_b));
    printf("cache %u items (%.0f MiB) · dataset %u items (%.2f GiB) · VRAM free %.1f GiB\n",
           cache_items, cache.size() * 4.0 / (1 << 20), dataset_items,
           dataset_bytes / (double)(1ull << 30), free_b / (double)(1ull << 30));

    uint32_t *d_cache, *d_dataset;
    CHECK(cudaMalloc(&d_cache, cache.size() * 4));
    CHECK(cudaMalloc(&d_dataset, dataset_bytes));
    CHECK(cudaMemcpy(d_cache, cache.data(), cache.size() * 4, cudaMemcpyHostToDevice));

    cudaEvent_t t0, t1;
    CHECK(cudaEventCreate(&t0));
    CHECK(cudaEventCreate(&t1));
    int threads = 128;
    int blocks = (int)((dataset_items + threads - 1) / threads);
    CHECK(cudaEventRecord(t0));
    k_dag_build<<<blocks, threads>>>(d_cache, cache_items, d_dataset, dataset_items);
    CHECK(cudaEventRecord(t1));
    CHECK(cudaDeviceSynchronize());
    CHECK(cudaGetLastError());
    float ms = 0;
    CHECK(cudaEventElapsedTime(&ms, t0, t1));
    printf("generation: %.0f ms → %.2f GiB/s\n", ms,
           dataset_bytes / (double)(1ull << 30) / (ms / 1000.0));

    snprintf(p, sizeof p, "%s/spotcheck.txt", dir);
    FILE *f = fopen(p, "r");
    if (!f) { fprintf(stderr, "cannot open spotcheck.txt\n"); return 2; }
    char line[1024];
    int ok = 0, bad = 0;
    uint32_t got[DAG_ITEM_WORDS];
    while (fgets(line, sizeof line, f)) {
        unsigned long long idx;
        char hexbuf[600];
        if (sscanf(line, "%llu %599s", &idx, hexbuf) != 2) continue;
        if (idx >= dataset_items) continue;
        CHECK(cudaMemcpy(got, d_dataset + idx * DAG_ITEM_WORDS, DAG_ITEM_BYTES,
                         cudaMemcpyDeviceToHost));
        std::string h;
        static const char *H = "0123456789abcdef";
        for (int i = 0; i < DAG_ITEM_WORDS; i++) {
            for (int b = 0; b < 4; b++) {
                uint8_t v = (uint8_t)(got[i] >> (b * 8));
                h += H[v >> 4];
                h += H[v & 15];
            }
        }
        if (h == std::string(hexbuf)) {
            ok++;
        } else {
            if (bad < 3) printf("BAD idx=%llu\n  want %.64s...\n  got  %.64s...\n", idx, hexbuf,
                                h.c_str());
            bad++;
        }
    }
    fclose(f);
    printf("spot checks: %d OK, %d bad\n", ok, bad);
    return bad ? 1 : 0;
}
