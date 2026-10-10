# PowMachina — a build without toolchains on the host.
#
#   docker build -t powmachina .
#   docker run --rm powmachina -selfcheck
#   docker run --rm -v powm-data:/data powmachina -regtest -datadir /data
#   docker run --rm -it -v ./data/wallet:/data --entrypoint powmachina-wallet-cli powmachina \
#       --regtest --generate-new-wallet /data/exchange

FROM golang:1.26-bookworm AS build

# RandomX is C++ and is compiled through cgo right in `go build`.
RUN apt-get update && apt-get install -y --no-install-recommends build-essential \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /src

# The manifests first, so that the dependency layer is cached.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=1 go build -trimpath -ldflags="-s -w" -o /out/ \
        ./cmd/powmachinad ./cmd/powmachina-wallet-cli ./cmd/powmachina-wallet-rpc \
        ./cmd/powmachina-miner ./cmd/powmachina-finality-signer

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates libstdc++6 \
    && rm -rf /var/lib/apt/lists/* \
    && useradd -r -u 10001 -m -d /data powm
# The node, the wallet, the miner and the quorum signer in one image; which program runs is decided by
# the `entrypoint` in compose. The signer is deliberately started on a **different machine than the
# node** (the finality spec, §5) — the image is the same, the deployment is not.
COPY --from=build /out/ /usr/local/bin/
# BSD-3 (ours and RandomX's) asks for a copyright notice in binary distribution too — an image is one.
COPY --from=build /src/LICENSE /src/NOTICE /usr/share/doc/powmachina/

# The data in a persistent volume — without it a restart would mean a resync.
VOLUME /data
USER powm
WORKDIR /data

# The regtest P2P and RPC. The ports are **not translated** — the same number on both sides.
EXPOSE 27696 27697

ENTRYPOINT ["powmachinad"]
CMD ["-h"]
