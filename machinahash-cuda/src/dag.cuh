// Generating DAG items from the cache — a port of `Cache.datasetItem` from internal/mh/dag.go.
//
// The cache is built on the CPU (it is serial, every item depends on the previous one) and uploaded to
// VRAM. The GPU computes the dataset itself: 256 independent reads from the cache per item
// parallelize well as long as the cache fits in L2.
#pragma once
#include <stdint.h>

#include "blake3.cuh"
#include "params.cuh"

// `xof(DOMAIN_ITEM, &[&idx.to_le_bytes(), &parent0], &mut seed)` — the domain, 8 B of index, 64 B of
// the parent. 95 B in total, so one BLAKE3 chunk.
__device__ __forceinline__ void mh_dag_item(uint32_t idx, const uint32_t *cache,
                                            uint32_t cache_items, uint32_t out[DAG_ITEM_WORDS]) {
    uint8_t buf[MH_DOMAIN_ITEM_LEN + 8 + CACHE_ITEM_BYTES];
#pragma unroll
    for (int i = 0; i < MH_DOMAIN_ITEM_LEN; i++) buf[i] = MH_DOMAIN_ITEM[i];

    uint64_t idx64 = idx;
#pragma unroll
    for (int i = 0; i < 8; i++) buf[MH_DOMAIN_ITEM_LEN + i] = (uint8_t)(idx64 >> (i * 8));

    const uint32_t *p0 = cache + (size_t)(idx % cache_items) * CACHE_ITEM_WORDS;
#pragma unroll
    for (int k = 0; k < CACHE_ITEM_WORDS; k++) {
#pragma unroll
        for (int b = 0; b < 4; b++) {
            buf[MH_DOMAIN_ITEM_LEN + 8 + k * 4 + b] = (uint8_t)(p0[k] >> (b * 8));
        }
    }

    uint8_t seed[DAG_ITEM_BYTES];
    b3_xof(buf, MH_DOMAIN_ITEM_LEN + 8 + CACHE_ITEM_BYTES, seed, DAG_ITEM_BYTES);
#pragma unroll
    for (int i = 0; i < DAG_ITEM_WORDS; i++) {
        out[i] = (uint32_t)seed[i * 4] | ((uint32_t)seed[i * 4 + 1] << 8) |
                 ((uint32_t)seed[i * 4 + 2] << 16) | ((uint32_t)seed[i * 4 + 3] << 24);
    }

    for (uint32_t j = 0; j < DAG_PARENTS; j++) {
        uint32_t p = mh_fnv1a(idx ^ j, out[j % DAG_ITEM_WORDS]) % cache_items;
        const uint32_t *parent = cache + (size_t)p * CACHE_ITEM_WORDS;
        uint32_t base = (j % 4) * CACHE_ITEM_WORDS;
#pragma unroll
        for (int k = 0; k < CACHE_ITEM_WORDS; k++) {
            out[base + k] = mh_fnv1a(out[base + k], parent[k]);
        }
    }

    // the final xof: the domain + "fin" + 256 B of mix = 282 B, again one chunk
    uint8_t fbuf[MH_DOMAIN_ITEM_LEN + 3 + DAG_ITEM_BYTES];
#pragma unroll
    for (int i = 0; i < MH_DOMAIN_ITEM_LEN; i++) fbuf[i] = MH_DOMAIN_ITEM[i];
    fbuf[MH_DOMAIN_ITEM_LEN + 0] = 'f';
    fbuf[MH_DOMAIN_ITEM_LEN + 1] = 'i';
    fbuf[MH_DOMAIN_ITEM_LEN + 2] = 'n';
#pragma unroll
    for (int i = 0; i < DAG_ITEM_WORDS; i++) {
#pragma unroll
        for (int b = 0; b < 4; b++) {
            fbuf[MH_DOMAIN_ITEM_LEN + 3 + i * 4 + b] = (uint8_t)(out[i] >> (b * 8));
        }
    }
    uint8_t fin[DAG_ITEM_BYTES];
    b3_xof(fbuf, MH_DOMAIN_ITEM_LEN + 3 + DAG_ITEM_BYTES, fin, DAG_ITEM_BYTES);
#pragma unroll
    for (int i = 0; i < DAG_ITEM_WORDS; i++) {
        out[i] = (uint32_t)fin[i * 4] | ((uint32_t)fin[i * 4 + 1] << 8) |
                 ((uint32_t)fin[i * 4 + 2] << 16) | ((uint32_t)fin[i * 4 + 3] << 24);
    }
}

// One thread = one dataset item.
__global__ void k_dag_build(const uint32_t *cache, uint32_t cache_items, uint32_t *dataset,
                            uint32_t dataset_items) {
    uint32_t i = blockIdx.x * blockDim.x + threadIdx.x;
    if (i >= dataset_items) return;
    uint32_t item[DAG_ITEM_WORDS];
    mh_dag_item(i, cache, cache_items, item);
    uint32_t *dst = dataset + (size_t)i * DAG_ITEM_WORDS;
#pragma unroll
    for (int k = 0; k < DAG_ITEM_WORDS; k++) dst[k] = item[k];
}
