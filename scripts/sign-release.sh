#!/usr/bin/env bash
# Signs a release that CI built, and attaches the signature to it.
#
#   scripts/sign-release.sh v0.1.0
#
# Why it is a separate step and not part of the workflow: the minisign key stays on the machine that
# signs. A key in a CI secret is a key that anyone with push access can use, and the whole point of
# signing a release is that **one** person with **one** key said "these bytes are mine". CI can be
# compromised; a laptop that never had the key uploaded anywhere cannot be compromised remotely.
#
# What it does: downloads the release's SHA256SUMS, checks that the files on the release match it,
# signs it, uploads the signature. It does **not** publish the release — look at it first, then
# `gh release edit <tag> --draft=false`.
set -euo pipefail
VERSION=${1:?usage: sign-release.sh <tag, e.g. v0.1.0>}
REPO=${REPO:-powmachina/powmachina}
SIGN_KEY=${SIGN_KEY:-$HOME/.powmachina/release.key}

command -v gh >/dev/null || { echo "gh is not installed — see the comment in this script" >&2; exit 1; }
[ -f "$SIGN_KEY" ] || { echo "the signing key $SIGN_KEY does not exist" >&2; exit 1; }

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
cd "$work"

echo "== downloading $VERSION from $REPO"
gh release download "$VERSION" --repo "$REPO" --pattern '*'

[ -f SHA256SUMS ] || { echo "the release has no SHA256SUMS" >&2; exit 1; }

# The list is signed, so what it describes has to be what is actually there. A file added to the
# release after CI built it would otherwise be covered by a signature that never saw it.
echo "== checking the files against the list"
# The list is relative to the release's flat layout, and gh downloads it flat too.
sha256sum --ignore-missing -c SHA256SUMS
missing=0
while read -r _ name; do
	[ -f "$name" ] || { echo "the list mentions $name, which is not on the release"; missing=1; }
done < SHA256SUMS
extra=$(comm -13 <(awk '{print $2}' SHA256SUMS | sort) <(ls -1 | grep -v '^SHA256SUMS' | sort) || true)
if [ -n "$extra" ]; then
	echo "these files are on the release but not in the list:" >&2
	echo "$extra" >&2
	missing=1
fi
[ "$missing" -eq 0 ] || { echo "the list and the release do not match — not signing" >&2; exit 1; }

echo "== signing"
if command -v minisign >/dev/null; then
	minisign -S -s "$SIGN_KEY" -m SHA256SUMS -t "PowMachina $VERSION"
else
	docker run --rm -i -v "$(dirname "$SIGN_KEY")":/keys -v "$work":/out \
		jedisct1/minisign -S -s "/keys/$(basename "$SIGN_KEY")" \
		-m /out/SHA256SUMS -t "PowMachina $VERSION"
	docker run --rm -v "$work":/out alpine chown "$(id -u):$(id -g)" /out/SHA256SUMS.minisig
fi

echo "== uploading the signature"
gh release upload "$VERSION" SHA256SUMS.minisig --repo "$REPO" --clobber

echo
echo "signed. Look at the release, then publish it:"
echo "  gh release edit $VERSION --repo $REPO --draft=false"
