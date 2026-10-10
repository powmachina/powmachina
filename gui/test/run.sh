#!/usr/bin/env bash
# Runs the wallet page in a headless browser against a stand-in backend (mock.js) and drives it like
# a person would (drive.js): wrong passwords, mistyped addresses, a double click on Send, an outage.
# The page under test is the real gui/src; nothing here is shipped.
#
#   gui/test/run.sh                 # needs Chromium, Chrome or Edge; BROWSER=/path/to/binary to pick one
#
# e2e/run.js is the other half: the same page against a real regtest node and wallet-rpc.
set -euo pipefail
ROOT=$(cd "$(dirname "$0")/.." && pwd)
BROWSER=${BROWSER:-$(command -v chromium || command -v chromium-browser || command -v google-chrome || command -v msedge || true)}
[ -n "$BROWSER" ] || { echo "no browser found — set BROWSER=/path/to/chromium" >&2; exit 2; }
T=$(mktemp -d); trap 'rm -rf "$T"' EXIT
cp -r "$ROOT/src/." "$T/"; cp "$ROOT/test/mock.js" "$ROOT/test/drive.js" "$T/"
sed -i 's#<script src="app.js"></script>#<script src="mock.js"></script><script src="app.js"></script><script src="drive.js"></script>#' "$T/index.html"
fail=0
if command -v node >/dev/null; then node "$ROOT/test/qr.test.js" || fail=1; else echo "qr.test.js skipped — no node"; fi
for suite in t-all t-outage t-create,fresh; do
	out=$("$BROWSER" --headless=new --disable-gpu --user-data-dir="$T/profile-$suite" --virtual-time-budget=20000 \
		--dump-dom "file://$T/index.html#$suite" 2>/dev/null | tr -d '\r')
	title=$(printf '%s' "$out" | grep -o '<title>[^<]*' | sed 's/<title>//')
	echo "$suite: $title"
	case "$title" in "TESTS OK"*) ;; *) fail=1; printf '%s\n' "$out" | grep -E 'FAIL|TIMEOUT' || true ;; esac
done
exit $fail
