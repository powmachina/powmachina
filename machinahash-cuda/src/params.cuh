// The MachinaHash-G parameters — a mirror of internal/mh/params.go.
//
// Lane C has no constants here: it is RandomX and runs on the CPU.
//
// Every value here MUST match internal/mh; a difference of one constant = a network fork
// (inviolable project rule 1). That is why nothing is derived here, only copied.
#pragma once
#include <stdint.h>


#define DAG_ITEM_BYTES 256
#define DAG_ITEM_WORDS 64
#define CACHE_ITEM_BYTES 64
#define CACHE_ITEM_WORDS 16
#define MIX_WORDS 64
#define B_ACCESSES_PER_ROUND 64
#define DAG_PARENTS 256
#define RANDMATH_OPS 16

#define MH_FNV_PRIME 0x01000193u

__device__ __forceinline__ uint32_t mh_fnv1a(uint32_t h, uint32_t d) {
    return (h ^ d) * MH_FNV_PRIME;
}

// The domains — they have to be byte for byte the same as in internal/mh.
#define MH_DOMAIN_SEED_LEN 23
#define MH_DOMAIN_FINAL_LEN 24
#define MH_DOMAIN_BCORE_LEN 24
#define MH_DOMAIN_EPOCH_LEN 24
#define MH_DOMAIN_CACHE_LEN 24
#define MH_DOMAIN_ITEM_LEN 23

__device__ __constant__ char MH_DOMAIN_SEED[] = "PowMachina/MH-1/seed/v0";
__device__ __constant__ char MH_DOMAIN_FINAL[] = "PowMachina/MH-1/final/v0";
__device__ __constant__ char MH_DOMAIN_BCORE[] = "PowMachina/MH-1/bcore/v0";
__device__ __constant__ char MH_DOMAIN_EPOCH[] = "PowMachina/MH-1/epoch/v0";
__device__ __constant__ char MH_DOMAIN_CACHE[] = "PowMachina/MH-1/cache/v0";
__device__ __constant__ char MH_DOMAIN_ITEM[] = "PowMachina/MH-1/item/v0";

// The lengths are written by hand (we need them as constants for buffer sizes), so we let the compiler
// check them. A typo in a domain or in a length = a different hash = a network fork.
static_assert(sizeof(MH_DOMAIN_SEED) - 1 == MH_DOMAIN_SEED_LEN, "MH_DOMAIN_SEED_LEN");
static_assert(sizeof(MH_DOMAIN_FINAL) - 1 == MH_DOMAIN_FINAL_LEN, "MH_DOMAIN_FINAL_LEN");
static_assert(sizeof(MH_DOMAIN_BCORE) - 1 == MH_DOMAIN_BCORE_LEN, "MH_DOMAIN_BCORE_LEN");
static_assert(sizeof(MH_DOMAIN_EPOCH) - 1 == MH_DOMAIN_EPOCH_LEN, "MH_DOMAIN_EPOCH_LEN");
static_assert(sizeof(MH_DOMAIN_CACHE) - 1 == MH_DOMAIN_CACHE_LEN, "MH_DOMAIN_CACHE_LEN");
static_assert(sizeof(MH_DOMAIN_ITEM) - 1 == MH_DOMAIN_ITEM_LEN, "MH_DOMAIN_ITEM_LEN");
