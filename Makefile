# aupr Makefile
#
# Targets:
#   make build     - compile to ./bin/aupr
#   make install   - go install into $(GOBIN) or $GOPATH/bin (or ~/go/bin)
#   make test      - run unit tests
#   make vet       - go vet
#   make lint      - golangci-lint (if installed) + vet
#   make fmt       - gofmt -w
#   make tidy      - go mod tidy
#   make once      - build then run `aupr once`
#   make clean     - remove ./bin
#
# Version is stamped into the binary via -ldflags.

PKG          := github.com/dagster-io/aupr
CMD          := ./cmd/aupr
BIN_DIR      := bin
BIN          := $(BIN_DIR)/aupr
VERSION      ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS      := -ldflags "-X $(PKG)/internal/cli.Version=$(VERSION)"

GOBIN        ?= $(shell go env GOBIN)
ifeq ($(GOBIN),)
GOBIN        := $(shell go env GOPATH)/bin
endif

.PHONY: default build install uninstall test vet lint fmt tidy once clean help

default: build

help:
	@awk 'BEGIN{FS=":.*##"} /^[a-zA-Z_-]+:.*##/ {printf "  %-12s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

build: ## build ./bin/aupr
	@mkdir -p $(BIN_DIR)
	go build $(LDFLAGS) -o $(BIN) $(CMD)
	@echo "built $(BIN) ($(VERSION))"

install: ## go install into $GOBIN
	go install $(LDFLAGS) $(CMD)
	@echo "installed $(GOBIN)/aupr ($(VERSION))"

uninstall: ## remove installed binary
	rm -f $(GOBIN)/aupr

test: ## run unit tests
	go test ./...

vet: ## go vet
	go vet ./...

lint: vet ## golangci-lint (if installed) + vet
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run; \
	else \
		echo "golangci-lint not installed, skipping"; \
	fi

fmt: ## gofmt -w
	gofmt -w .

tidy: ## go mod tidy
	go mod tidy

once: build ## run `aupr once`
	$(BIN) once

clean: ## remove ./bin
	rm -rf $(BIN_DIR)
