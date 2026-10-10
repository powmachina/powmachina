// Verifies that the GPU generates a bit-identical dataset to internal/mh.
//
// The fixtures are made by `go run ./cmd/mh-dumpdag <dir>` (the test profile:
// a 1 MiB cache / 16,384 items, an 8 MiB dataset / 32,768 items). The WHOLE dataset is compared,
// not a sample — one bad item out of millions is exactly the kind of bug that would only show up in
// production as a rejected block.
#include <cstdio>
#include <cstdlib>
#include <cstring>
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
    if (!f) {
        fprintf(stderr, "cannot open %s\n", path);
        exit(2);
    }
    fseek(f, 0, SEEK_END);
    long n = ftell(f);
    fseek(f, 0, SEEK_SET);
    std::vector<uint32_t> v(n / 4);
    if (fread(v.data(), 1, n, f) != (size_t)n) {
        fprintf(stderr, "short read %s\n", path);
        exit(2);
    }
    fclose(f);
    return v;
}

int main(int argc, char **argv) {
    const char *dir = argc > 1 ? argv[1] : "fixtures";
    char p[512];
    snprintf(p, sizeof p, "%s/cache.bin", dir);
    auto cache = load(p);
    snprintf(p, sizeof p, "%s/dataset.bin", dir);
    auto want = load(p);

    uint32_t cache_items = cache.size() / CACHE_ITEM_WORDS;
    uint32_t dataset_items = want.size() / DAG_ITEM_WORDS;
    printf("cache %u items, dataset %u items\n", cache_items, dataset_items);

    uint32_t *d_cache, *d_dataset;
    CHECK(cudaMalloc(&d_cache, cache.size() * 4));
    CHECK(cudaMalloc(&d_dataset, want.size() * 4));
    CHECK(cudaMemcpy(d_cache, cache.data(), cache.size() * 4, cudaMemcpyHostToDevice));

    cudaEvent_t t0, t1;
    CHECK(cudaEventCreate(&t0));
    CHECK(cudaEventCreate(&t1));
    int threads = 128;
    int blocks = (dataset_items + threads - 1) / threads;
    CHECK(cudaEventRecord(t0));
    k_dag_build<<<blocks, threads>>>(d_cache, cache_items, d_dataset, dataset_items);
    CHECK(cudaEventRecord(t1));
    CHECK(cudaDeviceSynchronize());
    CHECK(cudaGetLastError());
    float ms = 0;
    CHECK(cudaEventElapsedTime(&ms, t0, t1));

    std::vector<uint32_t> got(want.size());
    CHECK(cudaMemcpy(got.data(), d_dataset, want.size() * 4, cudaMemcpyDeviceToHost));

    size_t bad_items = 0;
    for (uint32_t i = 0; i < dataset_items; i++) {
        if (memcmp(&got[(size_t)i * DAG_ITEM_WORDS], &want[(size_t)i * DAG_ITEM_WORDS],
                   DAG_ITEM_BYTES) != 0) {
            if (bad_items < 3) {
                printf("BAD item %u\n  want ", i);
                for (int k = 0; k < 8; k++) printf("%08x", want[(size_t)i * DAG_ITEM_WORDS + k]);
                printf("...\n  got  ");
                for (int k = 0; k < 8; k++) printf("%08x", got[(size_t)i * DAG_ITEM_WORDS + k]);
                printf("...\n");
            }
            bad_items++;
        }
    }

    double mib = want.size() * 4.0 / (1 << 20);
    printf("\ndataset: %zu bad of %u items · %.1f MiB in %.1f ms = %.0f MiB/s\n", bad_items,
           dataset_items, mib, ms, mib / (ms / 1000.0));
    return bad_items ? 1 : 0;
}
