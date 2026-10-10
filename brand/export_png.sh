#!/usr/bin/env bash
# PNG exports. rsvg-convert, because the mark and the coins do not share a viewBox and
# svgrender.py only understands the 512 grid.
set -euo pipefail
cd "$(dirname "$0")"
run() { if command -v rsvg-convert >/dev/null; then rsvg-convert "$@"; else
  docker run --rm -v "$PWD:/w" -w /w debian:stable-slim sh -c \
    "apt-get -qq update >/dev/null && apt-get -qq install -y librsvg2-bin >/dev/null 2>&1; rsvg-convert $*"; fi; }
mkdir -p png && rm -f png/*.png
for f in powm-mark powm-mark-white; do
  for s in 128 256 512 1024 2048; do run -w $s "$f.svg" -o "png/$f-$s.png"; done
done
for f in powm-square powm-square-white powm-coin powm-coin-inv powm-coin-ring; do
  for s in 32 64 128 256 512 1024; do run -w $s -h $s "$f.svg" -o "png/$f-$s.png"; done
done
echo "PNG: $(ls png | wc -l) files"
