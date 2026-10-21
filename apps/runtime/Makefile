# See top-level Makefile for rationale. `/usr/bin/env` does not exist
# on Windows; a bare `bash` is resolved through PATH (Git Bash, WSL,
# MSYS2 all qualify).
ifeq ($(OS),Windows_NT)
SHELL := bash
else
SHELL := /usr/bin/env bash
endif
.SHELLFLAGS := -eo pipefail -c

BIN_NAME ?= ahv-remote-console-runtime
BIN_DIR  ?= bin
GO       ?= go
VERSION  ?= $(or $(AHV_REMOTE_CONSOLE_VERSION),dev)
COMMIT   ?= $(or $(AHV_REMOTE_CONSOLE_COMMIT),unknown)
DATE     ?= $(or $(AHV_REMOTE_CONSOLE_DATE),$(shell date -u +%Y-%m-%dT%H:%M:%SZ))

GOOS    ?= $(shell $(GO) env GOOS)
GOARCH  ?= $(shell $(GO) env GOARCH)
EXT      :=
ifeq ($(GOOS),windows)
EXT := .exe
endif

LDFLAGS := -s -w \
	-X 'github.com/MarcoCarvin/ahv-remote-console/apps/runtime/internal/build.Version=$(VERSION)' \
	-X 'github.com/MarcoCarvin/ahv-remote-console/apps/runtime/internal/build.Commit=$(COMMIT)' \
	-X 'github.com/MarcoCarvin/ahv-remote-console/apps/runtime/internal/build.Date=$(DATE)'

.PHONY: build dev test lint vet clean tidy

build: ## Build the runtime binary for $GOOS/$GOARCH.
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) \
		$(GO) build -trimpath -ldflags "$(LDFLAGS)" \
		-o $(BIN_DIR)/$(BIN_NAME)-$(GOOS)-$(GOARCH)$(EXT) ./cmd/ahv-remote-console-runtime
	@cp $(BIN_DIR)/$(BIN_NAME)-$(GOOS)-$(GOARCH)$(EXT) $(BIN_DIR)/$(BIN_NAME)$(EXT)

dev:
	$(GO) run ./cmd/ahv-remote-console-runtime --log-level=debug --log-format=text

test:
	$(GO) test -race -cover ./...

lint: vet
	@command -v staticcheck >/dev/null 2>&1 || $(GO) install honnef.co/go/tools/cmd/staticcheck@latest
	staticcheck ./...

vet:
	$(GO) vet ./...

tidy:
	$(GO) mod tidy

clean:
	rm -rf $(BIN_DIR)
