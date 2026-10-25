# PowMachina Wallet — desktop wallet

Tauri v2 (Rust shell + web UI) on top of **two Go sidecars**: `powmachinad` (a light node of
its own) and `powmachina-wallet-rpc`. The app starts both on `127.0.0.1` with random tokens
at launch and talks to them over JSON-RPC.

**Why sidecars and not a rewrite in Rust:** keys, signatures, Argon2id, BIP39 and block
scanning stay in Go, which means **one implementation of the cryptography**. A second one
would be a second chance at a bug that costs money — and project rule 1 says the same about
the algorithm.

Rust is **only the shell** here: the window, the sidecar lifecycle, the random tokens. No
wallet logic. It is a deliberate exception to the "one language: Go" rule, paid for with
what Go cannot do on the desktop: a native window, an installer, signing and an auto-updater.

```
gui/
  src/              web UI (plain HTML/CSS/JS, no framework, no node_modules)
  src-tauri/
    src/lib.rs      sidecar startup, tokens, Tauri commands
    binaries/       the node and wallet-rpc for the target platform are copied here
    tauri.conf.json
```

## Bundled node

The wallet runs its own node rather than trusting a public RPC: a wallet that trusts someone
else's server also trusts it about *what it received*, because it does not verify proof of
work on the blocks it is handed. The node runs without `-listen` (outbound connections only),
so it needs no firewall hole.

`-dag=auto` is the default: while catching up it builds the full DAG (~1 GiB, 0.62 ms per
header; with RandomX's dataset the node peaks at ~4.7 GiB) and drops to light at the tip (26 MiB,
63 ms). With less than 6 GiB of memory available it stays light from the start. `POWM_DAG=light` stays frugal the
whole time, at the price of a slower first sync. `POWM_NODE=host:port` skips the bundled node
and uses an existing one (then `POWM_RPC_TOKEN` is required).

## Windows installer

The `wallet-gui` workflow on GitHub, run by hand through **Run workflow** with a platform
choice — "build everything" is a decision and not a default. A `wallet-v*` tag builds
everything.

Finished artifacts are attached to the GitHub release:
`powmachina-wallet/0.1.0` (installer, bare binary, `SHA256SUMS` + signature).

**The Windows sidecars must be linked statically** (`-extldflags=-static`). Without it
`wallet-rpc.exe` depends on `libgcc_s_seh-1.dll` and `libstdc++-6.dll`, which a clean Windows
does not have and the installer does not ship — the wallet would look as if its backend
refused to start. It only shows up when the sidecar is run from the installer on a clean
machine (or under wine).

## Build

Linux needs the `libwebkit2gtk-4.1-dev` headers; on a machine without them build in a
container (`scripts/gui-build.sh`). Windows and macOS are built by GitHub Actions on a tag.

The sidecars must sit in `src-tauri/binaries/<name>-<target-triple>`; `scripts/gui-sidecar.sh`
produces them.
