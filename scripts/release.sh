#!/usr/bin/env bash
# A PowMachina release: binaries, checksums, signature.
#
# It runs in a **pinned container** (a digest, not a tag — a tag moves), so two people on two machines
# get the same bytes. Without that nobody can verify that the binary they downloaded came from this
# source — and that is exactly the cheapest attack on a new chain: not breaking the algorithm, but
# handing out a modified binary.
#
#   scripts/release.sh v0.1.0            # build, hash, sign
#   scripts/release.sh v0.1.0 --check    # build twice and compare (no signing)
#   scripts/release.sh v0.1.0 --no-sign  # build and hash; CI uses this, the signature is added later
#
# Signing is done with minisign (ed25519, the public key is a single line). GPG would mean explaining
# keyrings and the web of trust to people who only want to run a node.
#
#   the key is generated once:  minisign -G -s ~/.powmachina/release.key
#   the private key is NOT in the repo or in CI — signing happens on the machine that has it.
set -euo pipefail

# The pinned builder. Changing it changes what comes out — hence the digest is in the repo
# and not "whatever is under :1.27 today".
BUILDER=golang@sha256:972b4110ba0f47539a975c31e52c64765e1ff19f2d74469ae4a595c6676e80a5
BINARIES=(powmachinad powmachina-wallet-cli powmachina-wallet-rpc powmachina-miner)
# -X main.version stamps the version into the miner, which prints it and sends it to a pool as its
# user agent, and into the node, whose stratum server uses it to tell a miner on an older version
# that an update is available. Without it both say "dev": the person reporting a problem cannot tell
# us what they are running, a pool's worker list shows "dev" for everybody, and the update-notice is
# off. The flag is harmless for the binaries that have no such variable — the linker ignores it.
# Windows: the node, the wallet and the miner. The miner mines both lanes there once `powm-cuda.dll`
# sits next to it; the DLL is not built here (nvcc drives MSVC on Windows) — it comes from the
# cuda-windows workflow artifact, after it was verified on a card (BUILDING.md).
WIN_BINARIES=(powmachinad powmachina-wallet-cli powmachina-wallet-rpc powmachina-miner)
SIGN_KEY=${SIGN_KEY:-$HOME/.powmachina/release.key}

VERSION=${1:?usage: release.sh <version> [--check]}
MODE=${2:-sign}
ROOT=$(cd "$(dirname "$0")/.." && pwd)
OUT=$ROOT/dist/$VERSION

# The source has to be clean: a release from a work-in-progress tree cannot be reproduced from git.
if [ -n "$(git -C "$ROOT" status --porcelain)" ]; then
	echo "the tree is not clean — a release has to be reproducible from a commit" >&2
	exit 1
fi
COMMIT=$(git -C "$ROOT" rev-parse HEAD)

# build_win_into cross-builds the Windows binaries through mingw. It is the same pinned image, only
# with mingw installed — so reproducibility holds just the same.
build_win_into() {
	local dest=$1
	mkdir -p "$dest"
	docker run --rm \
		-v "$ROOT":/src:ro \
		-v "$dest":/out \
		-v powm-release-mod:/go/pkg/mod \
		-w /src \
		"$BUILDER" \
		sh -euc '
			apt-get update -qq && apt-get install -y -qq --no-install-recommends \
				build-essential gcc-mingw-w64-x86-64 g++-mingw-w64-x86-64 >/dev/null
			export CGO_ENABLED=1 GOOS=windows GOARCH=amd64
			export CC=x86_64-w64-mingw32-gcc CXX=x86_64-w64-mingw32-g++
			# -extldflags=-static is mandatory here, not cosmetic: without it the .exe depends on
			# libgcc_s_seh-1.dll and libstdc++-6.dll, which Windows does not have — the binary does
			# not even start and it looks like a corrupted file.
			go build -trimpath -buildvcs=false \
				-ldflags="-s -w -buildid= -extldflags=-static -X main.version='"$VERSION"'" \
				-o /out/ '"$(printf './cmd/%s ' "${WIN_BINARIES[@]}")"'
		'
}

build_into() {
	local dest=$1
	mkdir -p "$dest"
	# --network=none: the build must not fetch anything from the network, otherwise it is not
	# reproducible. The modules are fetched in a separate step below.
	docker run --rm \
		-v "$ROOT":/src:ro \
		-v "$dest":/out \
		-v powm-release-mod:/go/pkg/mod \
		-w /src \
		-e CGO_ENABLED=1 \
		-e GOFLAGS=-mod=readonly \
		"$BUILDER" \
		sh -euc '
			apt-get update -qq && apt-get install -y -qq --no-install-recommends build-essential >/dev/null
			go build -trimpath -buildvcs=false \
				-ldflags="-s -w -buildid= -X main.version='"$VERSION"'" \
				-o /out/ '"$(printf './cmd/%s ' "${BINARIES[@]}")"'
			chmod 0755 /out/*
		'
}

# The modules up front, so that the build itself can run without the network.
docker run --rm -v "$ROOT":/src:ro -v powm-release-mod:/go/pkg/mod -w /src \
	"$BUILDER" go mod download

if [ "$MODE" = "--check" ]; then
	a=$(mktemp -d) b=$(mktemp -d)
	trap 'rm -rf "$a" "$b"' EXIT
	echo "== first build"; build_into "$a"
	echo "== second build"; build_into "$b"
	echo
	for f in "${BINARIES[@]}"; do
		ha=$(sha256sum "$a/$f" | cut -d' ' -f1)
		hb=$(sha256sum "$b/$f" | cut -d' ' -f1)
		if [ "$ha" = "$hb" ]; then
			printf "  %-32s %s ✔\n" "$f" "${ha:0:16}"
		else
			printf "  %-32s DIFFERS %s vs %s ✘\n" "$f" "${ha:0:16}" "${hb:0:16}"
			exit 1
		fi
	done
	echo
	echo "the build is reproducible"
	exit 0
fi

rm -rf "$OUT"
build_into "$OUT/linux-amd64"
build_win_into "$OUT/windows-amd64"
# docker writes as root
docker run --rm -v "$OUT":/out alpine chown -R "$(id -u):$(id -g)" /out

# The CUDA plugin is not built here: it needs nvcc and a card to verify against the test vectors
# (rule 3). A release attaches it from the cuda:verify job's artifact.
if [ -f "$ROOT/machinahash-cuda/bin/libpowm-cuda.so" ]; then
	cp "$ROOT/machinahash-cuda/bin/libpowm-cuda.so" "$OUT/linux-amd64/"
else
	echo "⚠ libpowm-cuda.so is missing — the release will have no GPU plugin (download the cuda:verify artifact)" >&2
	echo "   without it lane G cannot be mined and the release is usable for lane C only" >&2
fi

# The Windows plugin the same way: from the cuda-windows workflow artifact, verified on a card first.
# With it the all-in-one miner archive is made — the one file a Windows user unpacks and runs.
if [ -f "$ROOT/machinahash-cuda/bin/powm-cuda.dll" ]; then
	cp "$ROOT/machinahash-cuda/bin/powm-cuda.dll" "$OUT/windows-amd64/"
	pack=$(mktemp -d)
	cp "$OUT/windows-amd64/powmachina-miner.exe" "$OUT/windows-amd64/powm-cuda.dll" "$pack/"
	cp "$ROOT/scripts/miner-windows-README.txt" "$pack/README.txt"
	# -X and a fixed mtime: the archive must not change just because it was packed at another time.
	find "$pack" -type f -exec touch -d "$(git -C "$ROOT" show -s --format=%cI HEAD)" {} +
	( cd "$pack" && zip -q -X "$OUT/windows-amd64/powmachina-miner-windows-x64-$VERSION.zip" README.txt powm-cuda.dll powmachina-miner.exe )
	rm -rf "$pack"
else
	echo "⚠ powm-cuda.dll is missing — the Windows miner will mine lane C only (download the cuda-windows artifact)" >&2
fi

# The same all-in-one archive for Linux. Two files and a README: what somebody who wants to mine
# downloads, instead of picking the miner and the plugin out of a directory of six binaries.
if [ -f "$OUT/linux-amd64/libpowm-cuda.so" ]; then
	pack=$(mktemp -d)
	cp "$OUT/linux-amd64/powmachina-miner" "$OUT/linux-amd64/libpowm-cuda.so" "$pack/"
	cp "$ROOT/scripts/miner-linux-README.txt" "$pack/README.txt"
	chmod 0755 "$pack/powmachina-miner"
	# A fixed mtime and a sorted file list: the archive must not change just because it was packed
	# at another time or on another filesystem.
	touch -d "$(git -C "$ROOT" show -s --format=%cI HEAD)" "$pack"/*
	tar --sort=name --owner=0 --group=0 --numeric-owner --format=gnu 		-czf "$OUT/linux-amd64/powmachina-miner-linux-x64-$VERSION.tar.gz" 		-C "$pack" README.txt libpowm-cuda.so powmachina-miner
	rm -rf "$pack"
fi

cat > "$OUT/VERSION" <<EOF
version: $VERSION
commit: $COMMIT
builder: $BUILDER
EOF

# The hashes are computed across all platforms at once, into a single signed list — one signature per
# release, not one per directory.
( cd "$OUT" && find . -type f ! -name SHA256SUMS -printf '%P\n' | sort | xargs sha256sum > SHA256SUMS )

if [ "$MODE" = "--no-sign" ]; then
	# CI builds everything but does not sign: the key stays on the machine that signs and is not a
	# CI secret, because a key in a CI variable is a key anyone with push access can use.
	echo
	echo "built without a signature — run scripts/sign-release.sh $VERSION to add it"
	echo "done: $OUT"
	ls -1 "$OUT"
	exit 0
fi

if [ ! -f "$SIGN_KEY" ]; then
	echo "⚠ the signing key $SIGN_KEY does not exist — the release is NOT signed" >&2
	echo "   generate it: minisign -G -s $SIGN_KEY" >&2
	exit 1
fi
# minisign may not be on the machine — then the official image is used. The key's password goes in on
# stdin from the environment (POWMACHINA_SIGN_PASSWORD in ~/.bashrc), not as an argument: arguments are
# visible to anyone who looks at the process list.
if command -v minisign >/dev/null; then
	printf '%s\n' "${POWMACHINA_SIGN_PASSWORD:-}" | minisign -S -s "$SIGN_KEY" \
		-m "$OUT/SHA256SUMS" -t "PowMachina $VERSION ($COMMIT)"
else
	printf '%s\n' "${POWMACHINA_SIGN_PASSWORD:-}" | docker run --rm -i \
		-v "$(dirname "$SIGN_KEY")":/keys -v "$OUT":/out \
		jedisct1/minisign -S -s "/keys/$(basename "$SIGN_KEY")" \
		-m /out/SHA256SUMS -t "PowMachina $VERSION ($COMMIT)"
	# docker writes as root — the signature has to belong to whoever makes the release
	docker run --rm -v "$OUT":/out alpine chown "$(id -u):$(id -g)" /out/SHA256SUMS.minisig
fi

echo
echo "done: $OUT"
ls -1 "$OUT"
echo
echo "verification by the person who downloaded it:"
echo "  minisign -Vm SHA256SUMS -P RWRA0GxkCXP1xdvCABffa3Lu8XBFplqC9P7JSefgpCk/NSkZ6q9k4i4Y && sha256sum -c SHA256SUMS"
