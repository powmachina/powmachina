# The vendored RandomX — what, from where and why

`internal/randomx/csrc/randomx/` is **RandomX v2** by tevador. It is the only implementation
this project uses — we do not write our own (a hard rule of the project: a difference of one
constant = a network fork).

## The exact origin

| | |
|---|---|
| upstream | https://github.com/tevador/RandomX |
| commit | **615de64537e3a29125c9de96c6242b84c1391c2e** → `615de64` |
| message | "Add .note.GNU-stack to the assembly sources (#341)" |
| licence | BSD 3-clause (`randomx/LICENSE`) |

### Why master and not the tag `v2.0.1`

`v2.0.1` (10 May 2026) contains **a bug in the x86 JIT** (`src/jit_compiler_x86.cpp`), fixed only
by commit `7c761cf` (7 August 2026, PR #340): on the `RANDOMX_FLAG_V2` path `codePos` was advanced
by `readDatasetSize` (the size of the v1 block) instead of `readDatasetV2Size`.

**Checked: the bug is latent, not active.** The blocks `program_read_dataset.inc` and
`program_read_dataset_v2.inc` contain the same instructions, only in a different order, so they
have the same length and `v2.0.1` computes correctly — but by accident. It would break on the first
change to either of the two blocks, and the result would be overwritten machine code.

It is exactly the class of bug the X41 audit warned about ("a single misplaced bit can emit an
entirely different instruction"). That is why we pin past the fix. `615de64` also adds
`.note.GNU-stack` to the assembly sources, i.e. hardening against an executable stack.

## Our only change

`powmachina.patch` — **6 added lines in `src/configuration.h`**, nothing else:

```c
#ifndef RANDOMX_ARGON_SALT
#define RANDOMX_ARGON_SALT         "RandomX\x03"
#endif
```

Without the definition the behaviour is **byte-identical to upstream**. That lets the same
vendored source be built in "stock" mode too and checked against the official RandomX test
vectors — that our build is right, before we hand it our own salt.

The salt is set by `../mh_salt.h`, which is pulled into every translation unit through `-include`.
A build with the stock salt is made with a build tag:

```sh
go test -tags mh_stock_salt ./internal/randomx/
```

## How to verify it

```sh
git clone https://github.com/tevador/RandomX && cd RandomX && git checkout 615de64
diff -r src ../powmachina/internal/randomx/csrc/randomx/src   # configuration.h only
cd ../powmachina/internal/randomx/csrc/randomx
sha256sum -c MANIFEST.sha256                                   # nothing changed since vendoring
```

## How it is compiled

cgo compiles **only the files directly in the package directory**, and the vendored sources are
meant to stay untouched. So `internal/randomx/` holds thin shim files that pull them in with
`#include`:

| shim | what it pulls in |
|---|---|
| `rx_all_amd64.cpp` / `rx_all_arm64.cpp` | all the C++ sources including the JIT for that architecture |
| `rx_all.c` | the C sources (Argon2 ref, blake2b, virtual_memory, reciprocal) |
| `rx_asm_amd64.S` / `rx_asm_arm64.S` | the static part of the JIT |
| `argon2ssse3/`, `argon2avx2/` | the Argon2 variants — packages of their own, because they need `-mssse3` / `-mavx2` and cgo gives CFLAGS to a whole package |

The C++ sources are therefore compiled as **a single translation unit**. That this changed nothing
is held by `randomx_conformance_test.go`: with the stock salt our build has to give exactly the
hashes upstream does.

## Why NOT our own parameters

The X41 audit (2019, OSTIF) found several problems with **non-standard parameters** — precisely
because the defaults are meant for Monero and other projects change them. Four independent audits
(X41 D-SEC, Kudelski, QuarksLab + one funded by Arweave, about €110,000 in total) verified the
**Monero defaults**.

That is why we change **the salt only**. It separates our network from Monero (a different cache
and dataset, so rented hashrate cannot be pointed at us), but leaves us exactly inside what those
audits covered.

The two coins that tuned the parameters the most — Wownero (a 1 MiB scratchpad) and Arqma
(256 KiB) — tuned them **in the opposite direction** from what we need: a smaller scratchpad means
a smaller CPU advantage.

## What follows for `AUDIT.md`

This is a trust assumption and belongs on the list:

- we trust RandomX v2 and its audits,
- **the node runs a JIT that generates and executes machine code** over data from blocks made by
  strangers. Hence `RANDOMX_FLAG_SECURE` (W^X), and the question whether light mode is enough
  for verification.
