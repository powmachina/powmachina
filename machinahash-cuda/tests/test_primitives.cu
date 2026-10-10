// A bit-exact test of the CUDA primitives against the reference values from internal/mh.
//
// BLAKE3 is compared directly: 16 input lengths x 3 XOF output lengths, including the 64 B block
// boundaries. It is the only primitive lane G needs.
//
// The reference values: `go run ./cmd/mh-refvals > machinahash-cuda/tests/refvals.txt`
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <string>
#include <vector>

#include "../src/blake3.cuh"

#define CHECK(x)                                                                          \
    do {                                                                                  \
        cudaError_t e = (x);                                                              \
        if (e != cudaSuccess) {                                                           \
            fprintf(stderr, "CUDA %s:%d: %s\n", __FILE__, __LINE__, cudaGetErrorString(e)); \
            exit(2);                                                                      \
        }                                                                                 \
    } while (0)

__global__ void k_b3(const uint8_t *in, uint32_t in_len, uint8_t *out, uint32_t out_len) {
    if (threadIdx.x || blockIdx.x) return;
    b3_xof(in, in_len, out, out_len);
}

static std::vector<uint8_t> unhex(const std::string &s) {
    std::vector<uint8_t> v(s.size() / 2);
    for (size_t i = 0; i < v.size(); i++) {
        auto d = [](char c) { return c <= '9' ? c - '0' : (c | 32) - 'a' + 10; };
        v[i] = (uint8_t)((d(s[i * 2]) << 4) | d(s[i * 2 + 1]));
    }
    return v;
}

static std::string hex(const uint8_t *b, size_t n) {
    static const char *H = "0123456789abcdef";
    std::string s;
    for (size_t i = 0; i < n; i++) {
        s += H[b[i] >> 4];
        s += H[b[i] & 15];
    }
    return s;
}

int main(int argc, char **argv) {
    const char *path = argc > 1 ? argv[1] : "tests/refvals.txt";
    FILE *f = fopen(path, "r");
    if (!f) {
        fprintf(stderr, "cannot open %s\n", path);
        return 2;
    }

    uint8_t *d_in, *d_out;
    CHECK(cudaMalloc(&d_in, 4096));
    CHECK(cudaMalloc(&d_out, 4096));

    char line[8192];
    int ok = 0, bad = 0;
    while (fgets(line, sizeof line, f)) {
        if (line[0] == '#' || line[0] == '\n') continue;
        char kind[8];
        if (sscanf(line, "%7s", kind) != 1) continue;

        if (!strcmp(kind, "b3")) {
            unsigned in_len, out_len;
            char a[8192] = {0}, b[8192] = {0};
            int n = sscanf(line, "%*s %u %u %s %s", &in_len, &out_len, a, b);
            // an input of length 0 => the array `a` is in fact the output
            std::string in_hex, exp_hex;
            if (in_len == 0) {
                in_hex = "";
                exp_hex = (n >= 3) ? a : "";
            } else {
                in_hex = a;
                exp_hex = b;
            }
            auto in = unhex(in_hex);
            if (in.size() != in_len) {
                fprintf(stderr, "a row whose input length does not match: %u vs %zu\n", in_len, in.size());
                bad++;
                continue;
            }
            if (in_len) CHECK(cudaMemcpy(d_in, in.data(), in_len, cudaMemcpyHostToDevice));
            k_b3<<<1, 1>>>(d_in, in_len, d_out, out_len);
            CHECK(cudaDeviceSynchronize());
            std::vector<uint8_t> got(out_len);
            CHECK(cudaMemcpy(got.data(), d_out, out_len, cudaMemcpyDeviceToHost));
            if (hex(got.data(), out_len) == exp_hex) {
                ok++;
            } else {
                bad++;
                printf("BAD b3 in_len=%u out_len=%u\n  expected %s\n  got      %s\n", in_len,
                       out_len, exp_hex.c_str(), hex(got.data(), out_len).c_str());
            }
        }
    }
    fclose(f);
    printf("\nprimitives: %d OK, %d bad\n", ok, bad);
    return bad ? 1 : 0;
}
