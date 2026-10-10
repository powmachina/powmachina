# machinahash-cuda

The CUDA kernel of **MachinaHash-G** (lane G). **The reference is `internal/mh`** — the kernel has to
be bit-exact, otherwise the GPU mines blocks the network rejects.

**Lane C is not here and will not be.** It is RandomX v2, that is CPU territory — the gate
decides that by measurement (the algorithm spec, §7.5).

## The build

The GPU machines deliberately do not have `nvcc`; compilation happens in a container, so that their
configuration does not change:

```sh
./build.sh tests/test_dag.cu test_dag              # sm_86 + sm_120
ARCH="-gencode arch=compute_86,code=sm_86" ./build.sh ...   # Ampere only
```

## The tests

The fixtures are generated from the reference and do not belong in the repo (`.gitignore`) — from the
repo's root:

```sh
go run ./cmd/mh-refvals > machinahash-cuda/tests/refvals.txt
go run ./cmd/mh-dumpdag <dir>              # the test profile: the cache + the whole dataset
MH_MAINNET=1 go run ./cmd/mh-dumpdag <dir> # mainnet: the cache + spot checks
```

| test | what it verifies |
|---|---|
| `test_primitives` | BLAKE3 (16 lengths × 3 XOF lengths) against the reference — lane G's only primitive |
| `test_dag` | the whole 8 MiB dataset of the test profile, item by item |
| `test_dag_big` | a 1 GiB dataset at the mainnet size + 64 spot checks |
| `test_lanes` | 500 lane G vectors (the lane C rows are skipped — that is RandomX) |
| `bench_laneg` | the lane G hashrate on the mainnet profile (gate 2) |

## The miner's plugin

`plugin/powm_cuda.cu` → `bin/libpowm-cuda.so` (`plugin/build.sh`) — the miner loads it through
dlopen. The search kernel is `src/mh1_fast.cuh`: a warp loads the DAG items together (16 threads ×
16 B = one coalesced 256 B read), and the BLAKE3 inputs are assembled word by word in registers.
`test_lanes` with `-DMH_FAST` verifies 500/500 vectors on this path too.

| | mh1 (byte buffers) | mh1_fast |
|---|---|---|
| RTX 3090 | 380 kH/s (43 %) | **847 kH/s (95 % of the ceiling)** |
| RTX PRO 6000 | 1.08 MH/s | **1.79 MH/s** |

## Measured

**500 of 500 lane G vectors match bit-exact on both cards** (sm_86 and sm_120).

### The hashrate, the mainnet profile (a 1 GiB DAG)

| | RTX PRO 6000 (a ceiling of 1,792 GB/s) | RTX 3090 (a ceiling of 936 GB/s) |
|---|---|---|
| **lane G** | **1,115,844 H/s · 1,170 GB/s · 65 % of the ceiling** | **390,867 H/s · 410 GB/s · 44 % of the ceiling** |

The CPU side (an EPYC 9474F, 96 threads): lane G 64,515 H/s. **Gate 2 = 17.3×** for the 6000, 6.1×
for the 3090 against the hardest CPU reference there is.

### Generating the DAG

| | RTX PRO 6000 | RTX 3090 |
|---|---|---|
| an 8 MiB dataset (the test one) | 0.6 ms · 12.7 GiB/s | 2.1 ms · 3.8 GiB/s |
| a 1 GiB dataset (mainnet) | **31 ms · 32.3 GiB/s** | **376 ms · 2.7 GiB/s** |

An epoch lasts ≈ 6.9 days, so the rebuild is negligible even on Ampere.

## Why the kernel looks the way it does

Two choices carry most of the hashrate:

**`mix` lives in shared memory, with a stride of 65** (bank padding). In global memory it would
cost 2.5 MiB of pointless traffic per hash against 1 MiB of useful DAG — 72 % of the bandwidth
wasted.

**A DAG item is read as 16× `uint4` through `__ldg`**, and a thread keeps no scratchpad: it needs
65 words of shared memory and VRAM holds the DAG alone, so nothing but the memory bus caps the
number of concurrent hashes.

**Measured and of no effect:** Lemire's fastmod instead of `% items` — the kernel is memory bound,
not ALU bound. It is kept; it helps on weaker cards.

## Notes on the port

- **BLAKE3 is trimmed to one chunk.** Every input in MH-1 is under 1 KiB (the longest is 290 B), so
  tree nodes are not needed. `b3_xof` has an assert for it — a longer input crashes the kernel
  instead of it silently returning a hash outside consensus.
- **The domain lengths are checked by `static_assert`** in `params.cuh`. A typo in a domain = a
  different hash = a fork.
