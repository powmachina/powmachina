# Building PowMachina

Everything but the CUDA plugin and the desktop wallet comes out of one command.

## The node, the miner and the wallet

You need **Go 1.23 or newer** and a **C compiler** — RandomX is C++ and is compiled through cgo as
part of the build.

```sh
./build.sh                  # or: make
./bin/powmachinad -selfcheck
```

`./build.sh test` builds and runs the whole test suite. `make conformance` additionally checks the
vendored RandomX against upstream's own hashes.

On Debian or Ubuntu the C side needs nothing beyond `build-essential`. On Windows, build in
git-bash with a mingw toolchain; `build.sh` exists precisely because `make` is often missing there.

### Without a toolchain at all

```sh
docker build -t powmachina .
docker run --rm powmachina -selfcheck
```

## The GPU plugin

Lane G runs through a plugin the miner loads at runtime — `libpowm-cuda.so` on Linux,
`powm-cuda.dll` on Windows. The miner itself does not link against CUDA, so a machine without a
card needs none of this.

### Linux

```sh
cd machinahash-cuda && ./build.sh
```

### Windows

`nvcc` drives MSVC there rather than mingw, which is fine: the plugin is loaded through
`LoadLibrary`, so there is no link-time coupling with the mingw-built `powmachina-miner.exe`. They
only have to agree on the plain-C ABI in `plugin/powm_cuda.h`.

In a *Developer Command Prompt for VS* with the CUDA toolkit installed:

```
cd machinahash-cuda
nvcc -O3 -std=c++17 ^
    -gencode arch=compute_75,code=sm_75 -gencode arch=compute_80,code=sm_80 ^
    -gencode arch=compute_86,code=sm_86 -gencode arch=compute_89,code=sm_89 ^
    -gencode arch=compute_90,code=sm_90 -gencode arch=compute_120,code=sm_120 ^
    -gencode arch=compute_120,code=compute_120 ^
    -shared -cudart static -o bin\powm-cuda.dll plugin\powm_cuda.cu
```

Two things in that command are not optional.

**The architecture list has to match the one the Linux plugin is built with**, or a release ends up
running on a different range of cards depending on which system built it. The trailing
`compute_120,code=compute_120` is the PTX fallback that lets a card newer than anything on the list
still run.

**`-cudart static`.** With it a user needs only an NVIDIA driver (580 or newer, same as on Linux).
Without it they would need a matching CUDA runtime DLL beside the plugin — exactly the kind of
thing that makes a miner fail to start with no explanation.

Compiling needs no card. Running does: the miner re-checks the kernel against `internal/mh` at
every epoch and switches off a card whose hashes disagree.

## The desktop wallet

The wallet is a [Tauri](https://tauri.app/) app and bundles the node and `powmachina-wallet-rpc` as
sidecars, which have to be built first:

```sh
scripts/gui-sidecar.sh
cd gui && npm install && npm run tauri build
```

Linux needs the `libwebkit2gtk-4.1-dev` headers; on a machine without them, `scripts/gui-build.sh`
does the whole thing in a container. The sidecars have to land in
`gui/src-tauri/binaries/<name>-<target-triple>` — the script takes care of that.

Windows and macOS installers are built by GitHub Actions on a tag.

## Releases

`scripts/release.sh <version>` assembles the archives and the checksum list. It refuses to build
from a tree with uncommitted changes, and it does not build the CUDA plugins — those come from CI,
after they have been verified on real hardware.

Releases are signed with **minisign**. The private key is not in this repository and not in CI: a
key in a CI secret is a key anyone with push access can use. Signing happens on the machine that
holds it, which is why the release workflow leaves a draft rather than publishing.

Verifying a download needs no trust in GitHub:

```sh
minisign -Vm SHA256SUMS -P <the public key>   # the hash list is ours
sha256sum -c SHA256SUMS                       # the files match the list
```

`scripts/verify-release.sh` does both in one go.
