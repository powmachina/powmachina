// libpowm-cuda — the PowMachina miner's GPU plugin (lane G, MachinaHash-G).
//
// The miner (Go) loads it at runtime through dlopen, so a single miner binary also runs on a machine
// without an NVIDIA card (CPU only). The interface is plain C; every function returns 0 on success,
// otherwise an error code and a message in `err`.
//
// The reference is internal/mh — Go computes the cache and the RANDMATH sequence and sends them here;
// the plugin builds the dataset from them on the card. At every epoch the miner verifies pmc_hash
// against internal/mh.
#pragma once
#include <stdint.h>

#ifdef __cplusplus
extern "C" {
#endif

#define PMC_ABI 1

// MSVC exports nothing from a DLL unless asked to; on Linux every extern "C" function is exported anyway.
#ifdef _WIN32
#define PMC_API __declspec(dllexport)
#else
#define PMC_API
#endif

PMC_API int pmc_abi(void);
PMC_API int pmc_device_count(void);
PMC_API int pmc_device_info(int dev, char *name, int name_cap, uint64_t *mem_bytes, int *sm_major,
                    int *sm_minor, int *sm_count, double *peak_gbs);

PMC_API void *pmc_create(int dev, char *err, int err_cap);
PMC_API void pmc_destroy(void *ctx);

// Builds the epoch dataset from the cache (cache_words words) and sets RANDMATH (ops: 16 x {kind,src,dst}).
// It then tunes the grid for this card automatically.
PMC_API int pmc_set_epoch(void *ctx, const uint32_t *cache, uint32_t cache_words, uint32_t dataset_items,
                  const uint8_t *ops, char *err, int err_cap);

// Sets the work: 80 B of header (without the nonce) and a 32 B target (big-endian, like mh.MeetsTarget).
PMC_API int pmc_set_job(void *ctx, const uint8_t *header80, const uint8_t *target32, char *err, int err_cap);

// Scans nonces from `start` (one launch, ~50 ms). It writes the nonces found into out (at most
// max_out), their count into *found and the number scanned into *hashes.
PMC_API int pmc_search(void *ctx, uint64_t start, uint64_t *out, uint32_t max_out, uint32_t *found,
               uint64_t *hashes, char *err, int err_cap);
// One lane G hash — for checking bit-exactness against internal/mh.
PMC_API int pmc_hash(void *ctx, const uint8_t *header80, uint64_t nonce, uint8_t *out32, char *err,
             int err_cap);

// The tuned grid (for printing).
PMC_API void pmc_config(void *ctx, int *threads_per_block, int *blocks, uint32_t *per_thread);

#ifdef __cplusplus
}
#endif
