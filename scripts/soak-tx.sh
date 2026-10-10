#!/usr/bin/env bash
# A soak test with real traffic: send a transaction every minute and watch whether the network
# keeps up with applying the bodies.
#
#   POWM_WALLET_RPC_TOKEN=… POWM_RPC_TOKEN=… scripts/soak-tx.sh 60 w03 tpowm1q…
#
# Why it exists: a block that carries transactions can fail to propagate while headers keep
# flowing. The chain looks healthy, and the UTXO set of every node stands at the hole — with it
# every wallet and the explorer. Unit tests that mine blocks with nothing but a coinbase and
# hand every peer every body cannot find that; real traffic can.
#
# What it watches is **`powm_header_body_gap`** on every node: headers run ahead of bodies for a
# few seconds after each block, and the gap has to close by itself. One that does not is the
# symptom that froze the wallets.
set -euo pipefail

MINUTES=${1:?usage: soak-tx.sh <minutes> <wallet> <destination address>}
WALLET=${2:?the wallet to send from (a name from listwallets)}
DEST=${3:?the destination address}
WALLET_RPC=${POWM_WALLET_RPC:-127.0.0.1:17695}
NODES=${POWM_NODES:-127.0.0.1}
METRICS_PORT=${POWM_METRICS_PORT:-17694}
AMOUNT=${POWM_SOAK_AMOUNT:-0.01}
# The gap that counts as a problem. A handful of blocks is ordinary right after a block, tens
# of them mean something is not arriving.
GAP_LIMIT=${POWM_SOAK_GAP:-10}
: "${POWM_WALLET_RPC_TOKEN:?the wallet-rpc token}"
: "${POWM_WALLET_PASSWORD:?the wallet password (needed for signing)}"

wrpc() {
	curl -sS -m 30 -H "Authorization: Bearer $POWM_WALLET_RPC_TOKEN" \
		-H 'Content-Type: application/json' \
		-d "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"$1\",\"params\":${2:-[]}}" \
		"http://$WALLET_RPC/wallet/$WALLET"
}

gap_of() { curl -sS -m 5 "http://$1:$METRICS_PORT/metrics" | grep -oP '^powm_header_body_gap \K[0-9.]+' || echo "?"; }

worst=0
sent=0
failed=0
echo "$(date -Is) soak: $MINUTES minutes, one transaction a minute from $WALLET, gap limit $GAP_LIMIT"
for ((i = 1; i <= MINUTES; i++)); do
	wrpc walletpassphrase "[\"$POWM_WALLET_PASSWORD\", 60]" >/dev/null
	out=$(wrpc sendtoaddress "[\"$DEST\", $AMOUNT]")
	wrpc walletlock >/dev/null
	txid=$(printf '%s' "$out" | python3 -c 'import sys,json;d=json.load(sys.stdin);print(d["result"] or "")')
	if [ -n "$txid" ]; then
		sent=$((sent + 1))
	else
		failed=$((failed + 1))
		echo "$(date -Is) send failed: $(printf '%s' "$out" | head -c 200)"
	fi

	line=""
	for n in $NODES; do
		g=$(gap_of "$n")
		line="$line $n=$g"
		case "$g" in
		'' | '?') continue ;;
		esac
		if [ "${g%.*}" -gt "$worst" ]; then worst=${g%.*}; fi
		if [ "${g%.*}" -gt "$GAP_LIMIT" ]; then
			echo "$(date -Is) ⚠ $n is $g blocks behind with its bodies"
		fi
	done
	echo "$(date -Is) [$i/$MINUTES] tx ${txid:0:12} gaps:$line"
	sleep 60
done

echo "$(date -Is) done: $sent sent, $failed failed, the largest header/body gap was $worst"
[ "$worst" -le "$GAP_LIMIT" ] || {
	echo "the gap exceeded the limit — see the sync design and the nodes' logs" >&2
	exit 1
}
