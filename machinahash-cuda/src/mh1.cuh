// MachinaHash-G on the GPU — a port of `Hasher.laneG` from internal/mh/hash.go.
//
// Lane G is all here. **Lane C is not here and does not belong here**: it is RandomX v2
// (`internal/randomx`), i.e. CPU territory. The gate decides it by measurement — a memory-hard
// core written for the CPU is computed faster by a 96 GB card than by a desktop Ryzen (ALGO §7.5).
//
// Lane G keeps no per-thread scratchpad, and that is deliberate: a 64 KiB scratchpad per thread
// would be 11 % of the memory traffic and, above all, would cap the number of concurrent hashes on
// a 24 GB card before the hashrate stops rising.
//
// `mix` (64 words) MUST point into **shared memory** with the stride MH_MIX_STRIDE (65, not 64).
// The padding shifts the banks: word `w` of thread `t` sits in bank `(t + w) % 32`. Without it a
// stride of 64 would send a whole warp into one bank. In global memory the mix cost 2.5 MiB of
// pointless traffic per hash against 1 MiB of useful DAG.
#pragma once
#include <stdint.h>

#include "blake3.cuh"
#include "params.cuh"

// The stride of a mix row in shared memory — 65, not 64, because of the banks (see the header).
#define MH_MIX_STRIDE (MIX_WORDS + 1)

/// The PoW preimage: header(80) ‖ nonce LE(8) ‖ lane(1).
#define MH_POW_PREIMAGE_BYTES 89
#define MH_NONCE_OFFSET 80
#define MH_LANE_OFFSET 88
#define MH_LANE_C 0
#define MH_LANE_G 1

struct MhMathOp {
    uint8_t kind, src, dst;
};

// The epoch's RANDMATH sequence — filled in from the host (derived in internal/mh, not here). KISS99
// in two implementations would be exactly the one-constant difference that splits a network.
__device__ __constant__ MhMathOp MH_OPS[RANDMATH_OPS];

// The division by the DAG item count happens 4,096 times per hash and `items` is not a power of two
// (the dataset grows by 4 MiB per epoch). Lemire's fastmod: one 64-bit multiplication + mulhi.
struct MhFastMod {
    uint64_t m;  // UINT64_MAX / d + 1
    uint32_t d;
};

__host__ __device__ __forceinline__ MhFastMod mh_fastmod_init(uint32_t d) {
    MhFastMod f;
    f.d = d;
    f.m = (uint64_t)(~0ull) / d + 1;
    return f;
}

__device__ __forceinline__ uint32_t mh_fastmod(uint32_t a, MhFastMod f) {
    return (uint32_t)__umul64hi(f.m * a, f.d);
}

__device__ __forceinline__ uint32_t mh_rotl32(uint32_t x, uint32_t n) {
    n &= 31;
    return n ? ((x << n) | (x >> (32 - n))) : x;
}
__device__ __forceinline__ uint32_t mh_rotr32(uint32_t x, uint32_t n) {
    n &= 31;
    return n ? ((x >> n) | (x << (32 - n))) : x;
}

__device__ __forceinline__ void mh_put_u32le(uint8_t *p, uint32_t v) {
    p[0] = (uint8_t)v; p[1] = (uint8_t)(v >> 8); p[2] = (uint8_t)(v >> 16); p[3] = (uint8_t)(v >> 24);
}
__device__ __forceinline__ uint32_t mh_get_u32le(const uint8_t *p) {
    return (uint32_t)p[0] | ((uint32_t)p[1] << 8) | ((uint32_t)p[2] << 16) | ((uint32_t)p[3] << 24);
}

// ---------------------------------------------------------------- B-core

__device__ __forceinline__ void mh_randmath(uint32_t *mix, uint32_t k) {
#pragma unroll
    for (int i = 0; i < RANDMATH_OPS; i++) {
        uint32_t d = ((uint32_t)MH_OPS[i].dst + k * 13u + (uint32_t)i) % MIX_WORDS;
        uint32_t s = ((uint32_t)MH_OPS[i].src + k * 7u + (uint32_t)i) % MIX_WORDS;
        uint32_t a = mix[d], b = mix[s], r;
        switch (MH_OPS[i].kind) {
            case 0: r = a + b; break;
            case 1: r = a * (b | 1u); break;
            case 2: r = a ^ b; break;
            case 3: r = mh_rotl32(a, b); break;
            case 4: r = mh_rotr32(a, b); break;
            case 5: r = a + (uint32_t)__clz((int)b); break;
            case 6: r = a ^ (uint32_t)__popc(b); break;
            case 7: {
                uint32_t lo = min(a, b), hi = max(a, b);
                r = lo + (hi >> 3);
                break;
            }
            default: r = (uint32_t)(((uint64_t)a * (uint64_t)b) >> 32); break;
        }
        mix[d] = r;
    }
}

__device__ __forceinline__ void mh_compress_round(uint32_t *mix) {
    uint8_t buf[MH_DOMAIN_BCORE_LEN + 5 + MIX_WORDS * 4];
    int o = 0;
#pragma unroll
    for (int i = 0; i < MH_DOMAIN_BCORE_LEN; i++) buf[o++] = MH_DOMAIN_BCORE[i];
    buf[o++] = 'r'; buf[o++] = 'o'; buf[o++] = 'u'; buf[o++] = 'n'; buf[o++] = 'd';
#pragma unroll
    for (int i = 0; i < MIX_WORDS; i++) { mh_put_u32le(buf + o, mix[i]); o += 4; }
    uint8_t out[MIX_WORDS * 4];
    b3_xof(buf, o, out, MIX_WORDS * 4);
#pragma unroll
    for (int i = 0; i < MIX_WORDS; i++) mix[i] = mh_get_u32le(out + i * 4);
}

__device__ void mh_b_core(const uint8_t seed[32], uint32_t rounds, uint32_t accesses,
                          const uint32_t *dag, MhFastMod items, uint32_t *mix, uint8_t out[32]) {
    uint8_t buf[MH_DOMAIN_BCORE_LEN + 32];
#pragma unroll
    for (int i = 0; i < MH_DOMAIN_BCORE_LEN; i++) buf[i] = MH_DOMAIN_BCORE[i];
#pragma unroll
    for (int i = 0; i < 32; i++) buf[MH_DOMAIN_BCORE_LEN + i] = seed[i];
    uint8_t mb[MIX_WORDS * 4];
    b3_xof(buf, MH_DOMAIN_BCORE_LEN + 32, mb, MIX_WORDS * 4);
#pragma unroll
    for (int i = 0; i < MIX_WORDS; i++) mix[i] = mh_get_u32le(mb + i * 4);

    for (uint32_t r = 0; r < rounds; r++) {
        for (uint32_t k = 0; k < accesses; k++) {
            uint32_t idx = mh_fastmod(mh_fnv1a(mix[(k * 7u) % MIX_WORDS] ^ k, r), items);
            // A DAG item is 256 B and `cudaMalloc` aligns to 256 B, so `idx * 256`
            // is 16 B aligned → 16 vector reads instead of 64 scalar ones.
            // `__ldg` sends the read through the read-only cache: the DAG does not change while hashing.
            const uint4 *item4 = (const uint4 *)(dag + (size_t)idx * DAG_ITEM_WORDS);
#pragma unroll
            for (int q = 0; q < MIX_WORDS / 4; q++) {
                uint4 v = __ldg(&item4[q]);
                mix[q * 4 + 0] = mh_fnv1a(mix[q * 4 + 0], v.x);
                mix[q * 4 + 1] = mh_fnv1a(mix[q * 4 + 1], v.y);
                mix[q * 4 + 2] = mh_fnv1a(mix[q * 4 + 2], v.z);
                mix[q * 4 + 3] = mh_fnv1a(mix[q * 4 + 3], v.w);
            }
            mh_randmath(mix, k);
        }
        mh_compress_round(mix);
    }

    uint8_t obuf[MH_DOMAIN_BCORE_LEN + 3 + MIX_WORDS * 4 + 4 + 4];
    int o = 0;
#pragma unroll
    for (int i = 0; i < MH_DOMAIN_BCORE_LEN; i++) obuf[o++] = MH_DOMAIN_BCORE[i];
    obuf[o++] = 'o'; obuf[o++] = 'u'; obuf[o++] = 't';
#pragma unroll
    for (int i = 0; i < MIX_WORDS; i++) { mh_put_u32le(obuf + o, mix[i]); o += 4; }
    mh_put_u32le(obuf + o, rounds); o += 4;
    mh_put_u32le(obuf + o, accesses); o += 4;
    b3_xof(obuf, o, out, 32);
}

// ---------------------------------------------------------------- lane G

#define G_B_ROUNDS 16u
#define G_B_ACCESSES (4u * B_ACCESSES_PER_ROUND)

/// One lane G hash. It needs no scratchpad — 65 words of shared memory per thread are enough.
__device__ void mh_hash_lane_g(const uint8_t header[80], uint64_t nonce, const uint32_t *dag,
                               MhFastMod items, uint32_t *mix, uint8_t out[32]) {
    // the preimage
    uint8_t p[MH_POW_PREIMAGE_BYTES];
#pragma unroll
    for (int i = 0; i < 80; i++) p[i] = header[i];
#pragma unroll
    for (int i = 0; i < 8; i++) p[MH_NONCE_OFFSET + i] = (uint8_t)(nonce >> (i * 8));
    p[MH_LANE_OFFSET] = MH_LANE_G;

    // seed = BLAKE3(DOMAIN_SEED ‖ the preimage)
    uint8_t sbuf[MH_DOMAIN_SEED_LEN + MH_POW_PREIMAGE_BYTES];
#pragma unroll
    for (int i = 0; i < MH_DOMAIN_SEED_LEN; i++) sbuf[i] = MH_DOMAIN_SEED[i];
#pragma unroll
    for (int i = 0; i < MH_POW_PREIMAGE_BYTES; i++) sbuf[MH_DOMAIN_SEED_LEN + i] = p[i];
    uint8_t seed[32];
    b3_xof(sbuf, MH_DOMAIN_SEED_LEN + MH_POW_PREIMAGE_BYTES, seed, 32);

    uint8_t state[32];
    mh_b_core(seed, G_B_ROUNDS, G_B_ACCESSES, dag, items, mix, state);

    uint8_t fbuf[MH_DOMAIN_FINAL_LEN + 32 + 32 + 1];
    int o = 0;
#pragma unroll
    for (int i = 0; i < MH_DOMAIN_FINAL_LEN; i++) fbuf[o++] = MH_DOMAIN_FINAL[i];
#pragma unroll
    for (int i = 0; i < 32; i++) fbuf[o++] = state[i];
#pragma unroll
    for (int i = 0; i < 32; i++) fbuf[o++] = seed[i];
    fbuf[o++] = MH_LANE_G;
    b3_xof(fbuf, o, out, 32);
}
