#!/usr/bin/env bash
# Builds the desktop wallet for Linux **in a container**.
#
# The reason: on Linux Tauri links against `libwebkit2gtk-4.1-dev`, and those headers are not on the
# machine that builds (the library itself is, so the result does run). Windows and macOS are built by GitHub
# Actions — Tauri cannot be cross-built.
#
#   scripts/gui-build.sh            # the binary (fast, for trying it out)
#   scripts/gui-build.sh bundle     # the .deb and AppImage too
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
MODE=${1:-build}

docker run --rm -v "$ROOT":/src -w /src/gui/src-tauri \
	-v powm-gui-cargo:/usr/local/cargo/registry \
	-v powm-gui-target:/src/gui/src-tauri/target \
	-e CARGO_HOME=/usr/local/cargo \
	rust:1-bookworm sh -euc '
		apt-get update -qq
		apt-get install -y -qq --no-install-recommends \
			libwebkit2gtk-4.1-dev libssl-dev libayatana-appindicator3-dev \
			librsvg2-dev patchelf file desktop-file-utils >/dev/null
		if [ "'"$MODE"'" = "bundle" ]; then
			cargo install tauri-cli --version "^2" --locked --quiet 2>/dev/null || true
			cargo tauri build
		else
			cargo build --release
		fi
	'
# target lives in a docker volume (otherwise the whole of Tauri would be rebuilt on every run),
# so the binary has to be pulled out — you would not find it on the machine otherwise.
docker run --rm -v powm-gui-target:/t -v "$ROOT/gui":/out alpine sh -euc '
	mkdir -p /out/build
	cp /t/release/powmachina-wallet-gui /out/build/ 2>/dev/null || true
	cp -r /t/release/bundle /out/build/ 2>/dev/null || true
	chown -R '"$(id -u):$(id -g)"' /out/build
'
echo "done — the binary is in gui/build/"
