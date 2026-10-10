// BLAKE3 for MachinaHash — deliberately trimmed to what MH-1 actually uses.
//
// Every input in MH-1 is under 1 KiB (the longest is 290 B: the domain + 256 B of mix + the lengths),
// so ONE chunk is enough: no tree nodes, no chunk chaining. The output is an XOF of up to 256 B.
//
// If a longer input were ever added, `b3_xof` would catch it with an assert — a silently wrong hash
// would be worse than a crash.
#pragma once
#include <assert.h>
#include <stdint.h>

#define B3_CHUNK_START 1u
#define B3_CHUNK_END 2u
#define B3_ROOT 8u
#define B3_MAX_INPUT 1024u

__device__ __constant__ uint32_t B3_IV[8] = {0x6A09E667u, 0xBB67AE85u, 0x3C6EF372u, 0xA54FF53Au,
                                             0x510E527Fu, 0x9B05688Cu, 0x1F83D9ABu, 0x5BE0CD19u};

__device__ __forceinline__ uint32_t b3_rotr(uint32_t x, int n) {
    return (x >> n) | (x << (32 - n));
}

__device__ __forceinline__ void b3_g(uint32_t *s, int a, int b, int c, int d, uint32_t mx,
                                     uint32_t my) {
    s[a] = s[a] + s[b] + mx;
    s[d] = b3_rotr(s[d] ^ s[a], 16);
    s[c] = s[c] + s[d];
    s[b] = b3_rotr(s[b] ^ s[c], 12);
    s[a] = s[a] + s[b] + my;
    s[d] = b3_rotr(s[d] ^ s[a], 8);
    s[c] = s[c] + s[d];
    s[b] = b3_rotr(s[b] ^ s[c], 7);
}

__device__ __forceinline__ void b3_round(uint32_t *s, const uint32_t *m) {
    b3_g(s, 0, 4, 8, 12, m[0], m[1]);
    b3_g(s, 1, 5, 9, 13, m[2], m[3]);
    b3_g(s, 2, 6, 10, 14, m[4], m[5]);
    b3_g(s, 3, 7, 11, 15, m[6], m[7]);
    b3_g(s, 0, 5, 10, 15, m[8], m[9]);
    b3_g(s, 1, 6, 11, 12, m[10], m[11]);
    b3_g(s, 2, 7, 8, 13, m[12], m[13]);
    b3_g(s, 3, 4, 9, 14, m[14], m[15]);
}

__device__ __forceinline__ void b3_permute(uint32_t *m) {
    const int P[16] = {2, 6, 3, 10, 7, 0, 4, 13, 1, 11, 12, 5, 9, 14, 15, 8};
    uint32_t t[16];
#pragma unroll
    for (int i = 0; i < 16; i++) t[i] = m[P[i]];
#pragma unroll
    for (int i = 0; i < 16; i++) m[i] = t[i];
}

// The full compression — it returns all 16 state words (the XOF needs them, chaining only the first 8).
__device__ __forceinline__ void b3_compress(const uint32_t cv[8], const uint32_t block[16],
                                            uint32_t block_len, uint64_t counter, uint32_t flags,
                                            uint32_t out[16]) {
    uint32_t s[16];
#pragma unroll
    for (int i = 0; i < 8; i++) s[i] = cv[i];
#pragma unroll
    for (int i = 0; i < 4; i++) s[8 + i] = B3_IV[i];
    s[12] = (uint32_t)counter;
    s[13] = (uint32_t)(counter >> 32);
    s[14] = block_len;
    s[15] = flags;

    uint32_t m[16];
#pragma unroll
    for (int i = 0; i < 16; i++) m[i] = block[i];

#pragma unroll
    for (int r = 0; r < 7; r++) {
        b3_round(s, m);
        if (r < 6) b3_permute(m);
    }
#pragma unroll
    for (int i = 0; i < 8; i++) {
        out[i] = s[i] ^ s[i + 8];
        out[i + 8] = s[i + 8] ^ cv[i];
    }
}

// Loads a 64 B block from little-endian bytes and zero-pads the rest.
__device__ __forceinline__ void b3_load_block(const uint8_t *in, uint32_t len, uint32_t block[16]) {
#pragma unroll
    for (int i = 0; i < 16; i++) block[i] = 0;
    for (uint32_t i = 0; i < len; i++) {
        block[i >> 2] |= ((uint32_t)in[i]) << ((i & 3) * 8);
    }
}

// One chunk (≤ 1024 B) → an XOF of `out_len` bytes (MH-1 needs at most 256).
__device__ void b3_xof(const uint8_t *in, uint32_t in_len, uint8_t *out, uint32_t out_len) {
    // Above one chunk this port would have to chain tree nodes. Better to crash than to silently
    // return a hash that does not match the reference.
    assert(in_len <= B3_MAX_INPUT);

    uint32_t cv[8];
#pragma unroll
    for (int i = 0; i < 8; i++) cv[i] = B3_IV[i];

    uint32_t nblocks = in_len == 0 ? 1u : (in_len + 63u) / 64u;
    uint32_t block[16];
    uint32_t last_len = 0;
    uint32_t last_flags = 0;
    uint32_t last_block[16];

    for (uint32_t b = 0; b < nblocks; b++) {
        uint32_t off = b * 64u;
        uint32_t len = (b + 1u == nblocks) ? (in_len - off) : 64u;
        if (in_len == 0) len = 0;
        uint32_t flags = 0;
        if (b == 0) flags |= B3_CHUNK_START;
        if (b + 1u == nblocks) flags |= B3_CHUNK_END;
        b3_load_block(in + off, len, block);

        if (b + 1u == nblocks) {
            // the last block is not compressed here — it is held for the root output
#pragma unroll
            for (int i = 0; i < 16; i++) last_block[i] = block[i];
            last_len = len;
            last_flags = flags;
        } else {
            uint32_t st[16];
            b3_compress(cv, block, len, 0, flags, st);
#pragma unroll
            for (int i = 0; i < 8; i++) cv[i] = st[i];
        }
    }

    uint32_t produced = 0;
    uint64_t ctr = 0;
    while (produced < out_len) {
        uint32_t st[16];
        b3_compress(cv, last_block, last_len, ctr, last_flags | B3_ROOT, st);
        uint32_t take = out_len - produced;
        if (take > 64u) take = 64u;
        for (uint32_t i = 0; i < take; i++) {
            out[produced + i] = (uint8_t)(st[i >> 2] >> ((i & 3) * 8));
        }
        produced += take;
        ctr++;
    }
}

__device__ __forceinline__ void b3_hash32(const uint8_t *in, uint32_t in_len, uint8_t out[32]) {
    b3_xof(in, in_len, out, 32);
}
