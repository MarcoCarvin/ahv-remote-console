# See top-level Makefile for rationale. `/usr/bin/env` does not exist
# on Windows; a bare `bash` is resolved through PATH (Git Bash, WSL,
# MSYS2 all qualify).
ifeq ($(OS),Windows_NT)
SHELL := bash
else
SHELL := /usr/bin/env bash
endif
.SHELLFLAGS := -eo pipefail -c

GO ?= go
BIN_DIR ?= bin

.PHONY: build test lint vet clean

build:
	@mkdir -p $(BIN_DIR)
	# In v1, the AHV plugin is linked into the runtime binary. This target
	# exists for consistency with the top-level Makefile and to support
	# future out-of-process plugin builds.
	@echo "ahv plugin is in-tree (linked into ahv-remote-console-runtime)"

test:
	$(GO) test -race -cover ./...

lint: vet
	@command -v staticcheck >/dev/null 2>&1 || $(GO) install honnef.co/go/tools/cmd/staticcheck@latest
	staticcheck ./...

vet:
	$(GO) vet ./...

clean:
	rm -rf $(BIN_DIR)
