#!/usr/bin/env bash
# PowMachina — builds the node and the tools into bin/.
#
# It does what `make` does, only without `make` — on minimal systems and on Windows (git-bash)
# you often do not have it.
#
#   ./build.sh          build everything
#   ./build.sh test     build and run all the tests
#
# Do not want Go on the machine either? `docker build -t powmachina .`
set -euo pipefail

cd "$(dirname "$0")"
mkdir -p bin

need() {
    command -v "$1" >/dev/null 2>&1 || {
        echo "$1 is missing — $2" >&2
        exit 1
    }
}
need go "install Go 1.26+: https://go.dev/dl  (or use 'docker build .')"
need cc "a C compiler is missing — RandomX is C++ (apt install build-essential)"

echo "[1/2] node…"
go build -o bin/powmachinad ./cmd/powmachinad

echo "[2/2] tools…"
for t in mh-bench mh-vectors mh-dumpdag mh-refvals; do
    go build -o "bin/$t" "./cmd/$t"
done

if [[ "${1:-}" == "test" ]]; then
    echo
    echo "── tests ──"
    go test ./...
    echo "── RandomX conformance against upstream ──"
    go test -tags mh_stock_salt ./internal/randomx/
fi

echo
echo "done:"
ls -la bin/
echo
echo "try:  ./bin/powmachinad -selfcheck"
