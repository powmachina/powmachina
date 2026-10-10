// Lane G against the test vectors from the repo (vectors/mh-test-vectors.txt).
//
// Lane C is not tested here and never will be: it is RandomX v2, i.e. CPU territory
// (ALGO §7.6). The lane C rows are skipped.
//
// The vectors are the algorithm's contract. When this test fails, the bug is in the kernel — the
// vectors are not adjusted to "make the test pass" (cmd/mh-vectors, the file header).
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <string>
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

__global__ void k_lane_g(const uint8_t *headers, const uint64_t *nonces, uint32_t n,
                         const uint32_t *dag, MhFastMod items, uint8_t *out) {
    uint32_t i = blockIdx.x * blockDim.x + threadIdx.x;
#if defined(MH_FAST)
    uint32_t *mix = smix + threadIdx.x * MHF_MIX_STRIDE;
#else
    uint32_t *mix = smix + threadIdx.x * MH_MIX_STRIDE;
#endif
#if defined(MH_FAST)
    // the miner's fast path (mh1_fast.cuh) — it needs a whole warp, the threads past the end compute
    // the last vector and write nothing
    uint32_t j = i < n ? i : n - 1;
    uint32_t hdr[20];
    for (int k = 0; k < 20; k++) hdr[k] = mh_get_u32le(headers + (size_t)j * 80 + k * 4);
    uint8_t h[32];
    mhf::hash_lane_g(hdr, nonces[j], dag, items, mix, h);
    if (i < n)
        for (int k = 0; k < 32; k++) out[(size_t)i * 32 + k] = h[k];
#else
    if (i >= n) return;
    mh_hash_lane_g(headers + (size_t)i * 80, nonces[i], dag, items, mix, out + (size_t)i * 32);
#endif
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

static void unhex_into(const char *s, uint8_t *out, size_t n) {
    auto d = [](char c) { return c <= '9' ? c - '0' : (c | 32) - 'a' + 10; };
    for (size_t i = 0; i < n; i++) out[i] = (uint8_t)((d(s[i * 2]) << 4) | d(s[i * 2 + 1]));
}

int main(int argc, char **argv) {
    const char *fixdir = argc > 1 ? argv[1] : "fixtures";
    const char *vecpath = argc > 2 ? argv[2] : "vectors/mh-test-vectors.txt";

    char p[512];
    snprintf(p, sizeof p, "%s/dataset.bin", fixdir);
    auto dataset = load_u32(p);
    uint32_t items_n = dataset.size() / DAG_ITEM_WORDS;
    MhFastMod items = mh_fastmod_init(items_n);

    snprintf(p, sizeof p, "%s/randmath.txt", fixdir);
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

    uint32_t *d_dag;
    CHECK(cudaMalloc(&d_dag, dataset.size() * 4));
    CHECK(cudaMemcpy(d_dag, dataset.data(), dataset.size() * 4, cudaMemcpyHostToDevice));

    FILE *vf = fopen(vecpath, "r");
    if (!vf) { fprintf(stderr, "cannot open %s\n", vecpath); return 2; }
    std::vector<uint8_t> headers, want;
    std::vector<uint64_t> nonces;
    int skipped_c = 0;
    char line[1024];
    while (fgets(line, sizeof line, vf)) {
        if (line[0] == '#') continue;
        char lane[4], hh[200], wh[80];
        unsigned long long nonce;
        if (sscanf(line, "%3s %199s %llu %79s", lane, hh, &nonce, wh) != 4) continue;
        if (!strcmp(lane, "C")) { skipped_c++; continue; }
        if (strcmp(lane, "G")) { fprintf(stderr, "unknown lane %s\n", lane); return 2; }
        size_t ho = headers.size(), wo = want.size();
        headers.resize(ho + 80);
        want.resize(wo + 32);
        unhex_into(hh, headers.data() + ho, 80);
        unhex_into(wh, want.data() + wo, 32);
        nonces.push_back(nonce);
    }
    fclose(vf);
    uint32_t n = nonces.size();
    printf("dataset %u items · %u lane G vectors (lane C skipped: %d — that is RandomX)\n",
           items_n, n, skipped_c);
    if (!n) { fprintf(stderr, "no lane G vectors\n"); return 2; }

    uint8_t *d_headers, *d_out;
    uint64_t *d_nonces;
    CHECK(cudaMalloc(&d_headers, headers.size()));
    CHECK(cudaMalloc(&d_nonces, n * 8));
    CHECK(cudaMalloc(&d_out, n * 32));
    CHECK(cudaMemcpy(d_headers, headers.data(), headers.size(), cudaMemcpyHostToDevice));
    CHECK(cudaMemcpy(d_nonces, nonces.data(), n * 8, cudaMemcpyHostToDevice));

    int threads = 32;
#if defined(MH_FAST)
    size_t smem = (size_t)threads * MHF_MIX_STRIDE * sizeof(uint32_t);
#else
    size_t smem = (size_t)threads * MH_MIX_STRIDE * sizeof(uint32_t);
#endif
    k_lane_g<<<(n + threads - 1) / threads, threads, smem>>>(d_headers, d_nonces, n, d_dag,
                                                             items, d_out);
    CHECK(cudaDeviceSynchronize());
    CHECK(cudaGetLastError());

    std::vector<uint8_t> got(n * 32);
    CHECK(cudaMemcpy(got.data(), d_out, n * 32, cudaMemcpyDeviceToHost));

    int total_ok = 0, total_bad = 0;
    for (uint32_t i = 0; i < n; i++) {
        if (!memcmp(&got[i * 32], &want[i * 32], 32)) {
            total_ok++;
        } else {
            if (total_bad < 2) {
                static const char *H = "0123456789abcdef";
                std::string a, b;
                for (int k = 0; k < 32; k++) {
                    a += H[want[i * 32 + k] >> 4]; a += H[want[i * 32 + k] & 15];
                    b += H[got[i * 32 + k] >> 4];  b += H[got[i * 32 + k] & 15];
                }
                printf("BAD vector %u\n  want %s\n  got  %s\n", i, a.c_str(), b.c_str());
            }
            total_bad++;
        }
    }

    printf("\nlane G: %d OK, %d bad\n", total_ok, total_bad);
    return total_bad ? 1 : 0;
}
