#!/bin/bash
# Built through docker — the GPU machines have no nvcc and we do not touch their configuration.
set -euo pipefail
IMG=nvidia/cuda:13.0.1-devel-ubuntu24.04
ARCH=${ARCH:-"-gencode arch=compute_86,code=sm_86 -gencode arch=compute_120,code=sm_120"}
SRC=${1:?usage: build.sh <file.cu> [output]}
OUT=${2:-$(basename "${SRC%.cu}")}
exec docker run --rm --gpus all -v "$PWD:/w" -w /w "$IMG" \
    nvcc -O3 -std=c++17 $ARCH -o "bin/$OUT" "$SRC"
