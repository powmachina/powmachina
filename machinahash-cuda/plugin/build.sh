#!/bin/bash
# Builds libpowm-cuda.so in a container (the machines have no nvcc and we do not touch their
# configuration).
# cudart is linked statically — the plugin only needs the driver (libcuda.so, CUDA 13 → driver >= 580).
# Architectures: Turing (sm_75) through Blackwell (sm_120) + PTX for future cards.
set -euo pipefail
cd "$(dirname "$0")/.."
IMG=${IMG:-nvidia/cuda:13.4.3-devel-ubuntu24.04}
ARCH=${ARCH:-"-gencode arch=compute_75,code=sm_75 -gencode arch=compute_80,code=sm_80 \
 -gencode arch=compute_86,code=sm_86 -gencode arch=compute_89,code=sm_89 \
 -gencode arch=compute_90,code=sm_90 -gencode arch=compute_120,code=sm_120 \
 -gencode arch=compute_120,code=compute_120"}
mkdir -p bin
exec docker run --rm -u "$(id -u):$(id -g)" -v "$PWD:/w" -w /w "$IMG" \
    nvcc -O3 -std=c++17 $ARCH -shared -Xcompiler -fPIC -cudart static \
    ${EXTRA:-} -Xptxas -v -o bin/${OUT:-libpowm-cuda.so} plugin/powm_cuda.cu
