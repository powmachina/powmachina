// MachinaHash-G on the GPU without byte buffers — the same function as mh_hash_lane_g (mh1.cuh),
// only the BLAKE3 inputs are assembled word by word straight from registers and the XOF output goes
// directly into the mix.
//
// Why: mh1.cuh assembles the 285 B input and the 256 B round-compression output as byte arrays on
// the stack (~620 B of local memory per thread). Those live in L1/L2 and compete with the DAG
// reads — which is why, with it, a grid with 128 threads per SM wins on an RTX 3090 and the card
// sits at 44 % of its memory ceiling.
//
// Equality with the reference: tests/test_lanes.cu (500 vectors from internal/mh) runs on this path
// too, and at every epoch the miner compares the card's hash with internal/mh. The domain strings are
// compile-time constants here; they have to be byte for byte the same as in params.cuh (the
// static_assert below checks at least the lengths, the vectors check that the contents agree).
#pragma once
#include <stdint.h>

#include "blake3.cuh"
#include "mh1.cuh"
#include "params.cuh"

// A mix row in shared memory: 68 words (272 B) — 16 B aligned, so FNV reads and writes the mix in
// uint4 units (LDS.128/STS.128). A quarter-warp (8 threads) doing 128-bit accesses hits banks
// 4t mod 32 → no conflicts. Against a 65-word row (scalar accesses) that is 4x fewer shared-memory
// instructions in the inner loop — Nsight pointed at MIO as the bottleneck.
#define MHF_MIX_STRIDE (MIX_WORDS + 4)

namespace mhf {

// A word from a string (little-endian), the bytes past the end are zeros — computed by the compiler.
__host__ __device__ constexpr uint32_t lw(const char *s, int n, int i) {
    return (uint32_t)(i < n ? (uint8_t)s[i] : 0) | ((uint32_t)(i + 1 < n ? (uint8_t)s[i + 1] : 0) << 8) |
           ((uint32_t)(i + 2 < n ? (uint8_t)s[i + 2] : 0) << 16) |
           ((uint32_t)(i + 3 < n ? (uint8_t)s[i + 3] : 0) << 24);
}

constexpr char SEED[] = "PowMachina/MH-1/seed/v0";            // 23
constexpr char FINAL[] = "PowMachina/MH-1/final/v0";          // 24
constexpr char BCORE[] = "PowMachina/MH-1/bcore/v0";          // 24
constexpr char BROUND[] = "PowMachina/MH-1/bcore/v0round";    // 29
constexpr char BOUT[] = "PowMachina/MH-1/bcore/v0out";        // 27
static_assert(sizeof(SEED) - 1 == MH_DOMAIN_SEED_LEN, "SEED");
static_assert(sizeof(FINAL) - 1 == MH_DOMAIN_FINAL_LEN, "FINAL");
static_assert(sizeof(BCORE) - 1 == MH_DOMAIN_BCORE_LEN, "BCORE");

// The words of the domain prefixes — computed by the compiler, on the card they live in constant memory.
#define MHF_L(str, i) lw(str, (int)sizeof(str) - 1, (i) * 4)
__device__ __constant__ uint32_t SEED_W[6] = {MHF_L(SEED, 0), MHF_L(SEED, 1), MHF_L(SEED, 2),
                                              MHF_L(SEED, 3), MHF_L(SEED, 4), MHF_L(SEED, 5)};
__device__ __constant__ uint32_t FINAL_W[6] = {MHF_L(FINAL, 0), MHF_L(FINAL, 1), MHF_L(FINAL, 2),
                                               MHF_L(FINAL, 3), MHF_L(FINAL, 4), MHF_L(FINAL, 5)};
__device__ __constant__ uint32_t BCORE_W[6] = {MHF_L(BCORE, 0), MHF_L(BCORE, 1), MHF_L(BCORE, 2),
                                               MHF_L(BCORE, 3), MHF_L(BCORE, 4), MHF_L(BCORE, 5)};
__device__ __constant__ uint32_t BROUND_W[8] = {MHF_L(BROUND, 0), MHF_L(BROUND, 1), MHF_L(BROUND, 2),
                                                MHF_L(BROUND, 3), MHF_L(BROUND, 4), MHF_L(BROUND, 5),
                                                MHF_L(BROUND, 6), MHF_L(BROUND, 7)};
__device__ __constant__ uint32_t BOUT_W[7] = {MHF_L(BOUT, 0), MHF_L(BOUT, 1), MHF_L(BOUT, 2), MHF_L(BOUT, 3),
                                              MHF_L(BOUT, 4), MHF_L(BOUT, 5), MHF_L(BOUT, 6)};

// An XOF over a single chunk whose words are supplied by `word(w)`: it chains the full blocks, holds
// the last block for the root output and produces `nout` 64-byte output blocks into `out(t, st)`.
template <int LEN, typename Word, typename Out>
__device__ __forceinline__ void xof_words(Word word, int nout, Out out) {
    constexpr int NB = (LEN + 63) / 64;
    constexpr int LAST = LEN - (NB - 1) * 64;
    uint32_t cv[8];
#pragma unroll
    for (int i = 0; i < 8; i++) cv[i] = B3_IV[i];
#pragma unroll
    for (int b = 0; b < NB - 1; b++) {
        uint32_t blk[16], st[16];
#pragma unroll
        for (int i = 0; i < 16; i++) blk[i] = word(b * 16 + i);
        b3_compress(cv, blk, 64, 0, b == 0 ? B3_CHUNK_START : 0u, st);
#pragma unroll
        for (int i = 0; i < 8; i++) cv[i] = st[i];
    }
    uint32_t last[16];
#pragma unroll
    for (int i = 0; i < 16; i++) last[i] = ((NB - 1) * 16 + i) * 4 < LEN ? word((NB - 1) * 16 + i) : 0u;
    const uint32_t flags = (NB == 1 ? B3_CHUNK_START : 0u) | B3_CHUNK_END | B3_ROOT;
    for (int t = 0; t < nout; t++) {
        uint32_t st[16];
        b3_compress(cv, last, LAST, (uint64_t)t, flags, st);
        out(t, st);
    }
}

// seed = XOF32(SEED ‖ header(80) ‖ nonce LE(8) ‖ lane(1)). The preimage is at offset 23.
__device__ __forceinline__ void seed_words(const uint32_t hdr[20], uint64_t nonce, uint32_t seed[8]) {
    auto q = [&](int k) -> uint32_t {  // a word of the preimage
        if (k < 20) return hdr[k];
        if (k == 20) return (uint32_t)nonce;
        if (k == 21) return (uint32_t)(nonce >> 32);
        if (k == 22) return (uint32_t)MH_LANE_G;
        return 0u;
    };
    auto word = [&](int w) -> uint32_t {
        if (w < 5) return SEED_W[w];
        if (w == 5) return SEED_W[5] | (q(0) << 24);
        return (q(w - 6) >> 8) | (q(w - 5) << 24);
    };
    xof_words<23 + 89>(word, 1, [&](int, const uint32_t *st) {
#pragma unroll
        for (int i = 0; i < 8; i++) seed[i] = st[i];
    });
}

// Round compression: mix <- XOF256(BCORE ‖ "round" ‖ mix). The mix is at offset 29 (a 3-byte shift).
__device__ __forceinline__ void round_words(uint32_t *mix) {
    auto m = [&](int k) -> uint32_t { return k < MIX_WORDS ? mix[k] : 0u; };
    auto word = [&](int w) -> uint32_t {
        if (w < 7) return BROUND_W[w];
        if (w == 7) return BROUND_W[7] | (m(0) << 8);
        return (m(w - 8) >> 24) | (m(w - 7) << 8);
    };
    xof_words<29 + 256>(word, 4, [&](int t, const uint32_t *st) {
#pragma unroll
        for (int i = 0; i < 16; i++) mix[t * 16 + i] = st[i];
    });
}

__device__ __forceinline__ void b_core(const uint32_t seed[8], const uint32_t *dag, MhFastMod items,
                                       uint32_t *mix, uint32_t state[8]) {
    // mix <- XOF256(BCORE ‖ seed): 56 B, one block
    auto iw = [&](int w) -> uint32_t { return w < 6 ? BCORE_W[w] : (w < 14 ? seed[w - 6] : 0u); };
    xof_words<24 + 32>(iw, 4, [&](int t, const uint32_t *st) {
#pragma unroll
        for (int i = 0; i < 16; i++) mix[t * 16 + i] = st[i];
    });

    // The warp reads the DAG together: an item (256 B) belongs to one thread, but 16 threads load it
    // 16 B at a time — one coalesced read of whole lines instead of 16 scattered ones.
    // One half of the warp takes the item of thread 2*it, the other 2*it+1; FNV writes straight into
    // the owner's mix (the mix is in shared memory). On the general path Nsight showed 50 % wasted
    // sectors. It requires the whole warp in the loop (all threads do the same number of hashes).
    const uint32_t lane = threadIdx.x & 31u;
    const uint32_t half = lane >> 4, part = lane & 15u;
    uint32_t *wbase = mix - lane * MHF_MIX_STRIDE;  // the row of thread 0 of this warp
    for (uint32_t r = 0; r < G_B_ROUNDS; r++) {
        for (uint32_t k = 0; k < G_B_ACCESSES; k++) {
            uint32_t idx = mh_fastmod(mh_fnv1a(mix[(k * 7u) % MIX_WORDS] ^ k, r), items);
            __syncwarp();
#pragma unroll
            for (int ph = 0; ph < 2; ph++) {
                uint4 v[8];
#pragma unroll
                for (int it = 0; it < 8; it++) {
                    uint32_t owner = (uint32_t)(ph * 8 + it) * 2u + half;
                    uint32_t oidx = __shfl_sync(0xffffffffu, idx, owner);
                    v[it] = __ldg((const uint4 *)(dag + (size_t)oidx * DAG_ITEM_WORDS) + part);
                }
#pragma unroll
                for (int it = 0; it < 8; it++) {
                    uint32_t owner = (uint32_t)(ph * 8 + it) * 2u + half;
                    uint4 *row = (uint4 *)(wbase + owner * MHF_MIX_STRIDE);
                    uint4 m = row[part];
                    m.x = mh_fnv1a(m.x, v[it].x);
                    m.y = mh_fnv1a(m.y, v[it].y);
                    m.z = mh_fnv1a(m.z, v[it].z);
                    m.w = mh_fnv1a(m.w, v[it].w);
                    row[part] = m;
                }
            }
            __syncwarp();
            mh_randmath(mix, k);
        }
        round_words(mix);
    }

    // state ← XOF32(BCORE ‖ "out" ‖ mix ‖ rounds LE ‖ accesses LE). The mix is at offset 27.
    auto e = [&](int k) -> uint32_t {
        if (k < MIX_WORDS) return mix[k];
        if (k == MIX_WORDS) return G_B_ROUNDS;
        if (k == MIX_WORDS + 1) return G_B_ACCESSES;
        return 0u;
    };
    auto ow = [&](int w) -> uint32_t {
        if (w < 6) return BOUT_W[w];
        if (w == 6) return BOUT_W[6] | (e(0) << 24);
        return (e(w - 7) >> 8) | (e(w - 6) << 24);
    };
    xof_words<27 + 256 + 8>(ow, 1, [&](int, const uint32_t *st) {
#pragma unroll
        for (int i = 0; i < 8; i++) state[i] = st[i];
    });
}

// One lane G hash; the output as 32 bytes (the big-endian comparison with the target is the caller's job).
__device__ __forceinline__ void hash_lane_g(const uint32_t hdr[20], uint64_t nonce, const uint32_t *dag,
                                            MhFastMod items, uint32_t *mix, uint8_t out[32]) {
    uint32_t seed[8], state[8];
    seed_words(hdr, nonce, seed);
    b_core(seed, dag, items, mix, state);
    // XOF32(FINAL ‖ state ‖ seed ‖ lane): 89 B
    auto fw = [&](int w) -> uint32_t {
        if (w < 6) return FINAL_W[w];
        if (w < 14) return state[w - 6];
        if (w < 22) return seed[w - 14];
        if (w == 22) return (uint32_t)MH_LANE_G;
        return 0u;
    };
    xof_words<24 + 32 + 32 + 1>(fw, 1, [&](int, const uint32_t *st) {
#pragma unroll
        for (int i = 0; i < 8; i++) {
            out[i * 4 + 0] = (uint8_t)st[i];
            out[i * 4 + 1] = (uint8_t)(st[i] >> 8);
            out[i * 4 + 2] = (uint8_t)(st[i] >> 16);
            out[i * 4 + 3] = (uint8_t)(st[i] >> 24);
        }
    });
}

}  // namespace mhf
