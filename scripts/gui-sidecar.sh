#!/usr/bin/env bash
# Builds `powmachina-wallet-rpc` as a sidecar of the desktop wallet.
#
# Tauri looks for a sidecar under a name with the **target triple** (`-x86_64-unknown-linux-gnu`
# and so on) — without it the build fails with "binary not found" even though the file exists.
#
#   scripts/gui-sidecar.sh                      # for this machine
#   scripts/gui-sidecar.sh x86_64-pc-windows-gnu
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
OUT="$ROOT/gui/src-tauri/binaries"
TRIPLE=${1:-$(rustc -vV | awk '/^host:/{print $2}')}
mkdir -p "$OUT"

case "$TRIPLE" in
	*windows*) GOOS=windows; EXT=.exe; CC=x86_64-w64-mingw32-gcc; CXX=x86_64-w64-mingw32-g++;;
	*darwin*)  GOOS=darwin;  EXT="";   CC=clang; CXX=clang++;;
	*)         GOOS=linux;   EXT="";   CC=gcc;   CXX=g++;;
esac

# The wallet needs cgo (RandomX is in the tree even though the wallet does not call it), so the cross
# build goes through mingw the same way as the release binaries.
# Two sidecars: the wallet and the node. The node is there so the app does not need a foreign server —
# the wallet does not verify the PoW of blocks from a node, so running its own node is the only way
# not to rely on somebody not lying to us about received payments.
LD="-s -w"
case "$TRIPLE" in *windows*) LD="$LD -extldflags=-static";; esac

for cmd in powmachina-wallet-rpc powmachinad; do
	CGO_ENABLED=1 GOOS=$GOOS GOARCH=amd64 CC=$CC CXX=$CXX \
		go build -trimpath -ldflags="$LD" \
		-o "$OUT/$cmd-$TRIPLE$EXT" "$ROOT/cmd/$cmd"
	echo "sidecar: $OUT/$cmd-$TRIPLE$EXT"
done
