#!/usr/bin/env bash
# Prints the checkpoint and MinChainWork to bake into `internal/chain/netparams.go`.
#
# What it is for: without these two values a **new node can be fed a cheap fake history**. The
# checkpoint says "at this height there must be this block", MinChainWork says "a chain with
# less work is not ours". Neither decides a block's
# validity — they are safeguards for the first download, not consensus.
#
#   scripts/bake-checkpoint.sh 127.0.0.1:17697 $POWM_RPC_TOKEN
#   scripts/bake-checkpoint.sh 127.0.0.1:27697 $TOKEN 500      # the height by hand
#
# Without the third argument it takes the **height confirmed by the finality quorum** — a checkpoint on
# a block the quorum signed is the least arbitrary choice we have.
#
# The values have to be **re-baked before every public release**: the deeper the checkpoint, the less
# history can be faked, and MinChainWork only makes sense when it is close to the network's actual
# work.
set -euo pipefail

RPC=${1:?usage: bake-checkpoint.sh <host:port> <token> [height]}
TOKEN=${2:?the RPC token is missing}
HEIGHT=${3:-}

rpc() {
  curl -fsS -X POST -H "Authorization: Bearer $TOKEN" \
    -d "{\"method\":\"$1\",\"params\":${2:-null},\"id\":1}" "http://$RPC/"
}

INFO=$(rpc getinfo)
TIP=$(printf '%s' "$INFO" | python3 -c 'import sys,json;print(json.load(sys.stdin)["result"]["height"])')
WORK=$(printf '%s' "$INFO" | python3 -c 'import sys,json;print(json.load(sys.stdin)["result"]["chainwork"])')
NET=$(printf '%s' "$INFO" | python3 -c 'import sys,json;print(json.load(sys.stdin)["result"]["network"])')

if [ -z "$HEIGHT" ]; then
  FIN=$(rpc getfinality | python3 -c 'import sys,json;d=json.load(sys.stdin)["result"];print(d.get("height",0) if d.get("finalized") else 0)')
  if [ "$FIN" = "0" ]; then
    echo "the quorum has not confirmed anything yet — pass a height as the third argument" >&2
    exit 1
  fi
  HEIGHT=$FIN
fi

# A checkpoint closer to the tip than the reorg limit could name a block that nodes may still
# legitimately reorganize away, and then old binaries and new ones would part for good.
REORG_LIMIT=500
if [ "$HEIGHT" -gt $((TIP - REORG_LIMIT)) ]; then
  if [ "$TIP" -le "$REORG_LIMIT" ]; then
    echo "the chain is at $TIP — too short for a checkpoint $REORG_LIMIT below the tip" >&2
    exit 1
  fi
  echo "height $HEIGHT is within $REORG_LIMIT of the tip ($TIP) — using $((TIP - REORG_LIMIT))" >&2
  HEIGHT=$((TIP - REORG_LIMIT))
fi

ID=$(rpc getblock "{\"height\":$HEIGHT}" | python3 -c 'import sys,json;print(json.load(sys.stdin)["result"]["id"])')

# MinChainWork is taken **from the tip**, not from the checkpoint: it answers the question "does this
# chain have at least as much work as the network had at release time", not "how much did it have at
# height X". A chain only grows, so the value stays valid later on too.
HEX=$(python3 -c "print(format(int('$WORK'), 'x'))")

cat <<OUT

# network $NET, tip $TIP, checkpoint at height $HEIGHT
# ($(date -Is))

		// Baked at height $TIP.
		MinChainWork: "$HEX",
		Checkpoints: []Checkpoint{
			{Height: $HEIGHT, ID: mustID("$ID")},
		},
OUT
