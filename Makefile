# PowMachina — one command builds everything.
#
# `make` is enough. One language, one toolchain: Go. RandomX is C++ and is compiled through cgo right
# in `go build`, so all you need on top is a C compiler (build-essential).
#
# Do not want a toolchain? `make docker` builds everything in a container.

SHELL := /bin/bash
BIN   := bin
GO    := go
TOOLS := mh-bench mh-vectors mh-dumpdag mh-refvals

.PHONY: all build node tools test conformance fuzz check fmt clean docker help

all: build

## build: builds the node and the tools into bin/
build: node tools

## node: the powmachinad daemon
node: | $(BIN)
	$(GO) build -o $(BIN)/powmachinad ./cmd/powmachinad
	@echo "→ $(BIN)/powmachinad"

## tools: mh-bench (calibration), mh-vectors, mh-dumpdag, mh-refvals
tools: | $(BIN)
	@for t in $(TOOLS); do \
		$(GO) build -o $(BIN)/$$t ./cmd/$$t && echo "→ $(BIN)/$$t"; \
	done

$(BIN):
	@mkdir -p $(BIN)

## test: all the tests — 1,000 test vectors, the node, the storage including kill -9
test:
	$(GO) test ./...

## conformance: the vendored RandomX with the stock salt against upstream's official vectors
conformance:
	$(GO) test -tags mh_stock_salt ./internal/randomx/ -v

## check: formatting and static analysis, the way it runs in CI
check:
	@test -z "$$(gofmt -l .)" || { gofmt -l .; exit 1; }
	$(GO) vet ./...

## fmt: formats the sources
fmt:
	gofmt -w .

## docker: builds in a container — the host does not even need Go
docker:
	docker build -t powmachina:dev .
	@echo "run: docker run --rm powmachina:dev -h"

clean:
	$(GO) clean -cache -testcache
	rm -rf $(BIN)

help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/## /  make /'
