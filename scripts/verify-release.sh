#!/usr/bin/env bash
# Verifying a downloaded release. This is a script for whoever downloaded a binary —
# not for us.
#
#   scripts/verify-release.sh ~/Downloads/powmachina-v0.1.0
#
# It verifies two things, in this order:
#   1. the signature of SHA256SUMS (that the hash list is from us),
#   2. the hashes of the files themselves (that the files match the list).
#
# The other order would be pointless: an unsigned hash list only proves that a file
# agrees with itself.
set -euo pipefail

# The public key of PowMachina releases. It is not secret and it is deliberately here, in the source —
# not on a website, which can be rewritten.
PUBKEY=${POWM_RELEASE_PUBKEY:-"RWRA0GxkCXP1xdvCABffa3Lu8XBFplqC9P7JSefgpCk/NSkZ6q9k4i4Y"}

DIR=${1:?usage: verify-release.sh <release directory>}
cd "$DIR"

# minisign is packaged everywhere (apt install minisign / brew install minisign). If you do not have
# it but do have docker, the official image is used — verifying the signature should not be skipped
# just because one tool is missing.
if command -v minisign >/dev/null; then
	msign() { minisign "$@"; }
elif command -v docker >/dev/null; then
	msign() { docker run --rm -v "$PWD":/d:ro jedisct1/minisign "$@"; }
	echo "minisign is not on this machine — using the jedisct1/minisign docker image"
else
	echo "minisign (apt install minisign / brew install minisign) and docker are both missing" >&2
	exit 1
fi
echo "== the signature of the hash list"
if command -v minisign >/dev/null; then
	msign -Vm SHA256SUMS -P "$PUBKEY"
else
	msign -Vm /d/SHA256SUMS -P "$PUBKEY"
fi

echo "== the file hashes"
sha256sum -c SHA256SUMS

echo
echo "the release is from the key holder and the files match."
echo "If you want to go further and verify that the binary really comes from this source, build it yourself:"
echo "  git checkout \$(grep ^commit VERSION | cut -d' ' -f2) && scripts/release.sh X --check"
