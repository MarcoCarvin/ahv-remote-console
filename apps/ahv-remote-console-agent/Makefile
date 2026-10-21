# See top-level Makefile for rationale. `/usr/bin/env` does not exist
# on Windows; a bare `bash` is resolved through PATH (Git Bash, WSL,
# MSYS2 all qualify).
ifeq ($(OS),Windows_NT)
SHELL := bash
else
SHELL := /usr/bin/env bash
endif
.SHELLFLAGS := -eo pipefail -c

BIN_NAME ?= ahv-remote-console-agent
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
	-X 'github.com/MarcoCarvin/ahv-remote-console/apps/ahv-remote-console-agent/internal/build.Version=$(VERSION)' \
	-X 'github.com/MarcoCarvin/ahv-remote-console/apps/ahv-remote-console-agent/internal/build.Commit=$(COMMIT)' \
	-X 'github.com/MarcoCarvin/ahv-remote-console/apps/ahv-remote-console-agent/internal/build.Date=$(DATE)'

# Windows: link as the GUI subsystem so NO console window is allocated when
# the agent runs. The bridge auto-starts from the HKCU Run key at guest
# login/reboot; a console-subsystem binary pops a visible terminal in the
# guest showing the agent's JSON logs every time (the "should be hidden"
# window). GUI-subsystem suppresses that; logs still go to the agent's log
# file (logpath_windows.go), not a console. This must apply to the
# single-target `build` path too — not just `all-platforms` — because dev
# cross-builds (make build GOOS=windows) are what get hand-installed into a
# test guest.
ifeq ($(GOOS),windows)
LDFLAGS += -H windowsgui
endif

# Cross-compile target matrix. Each row produces a static binary with
# CGO disabled so the agent has no runtime libc dependency in the
# guest. Operators or IT teams ship the one for their VM's OS+arch.
TARGETS := \
	linux/amd64 \
	windows/amd64 \
	windows/arm64

# Authenticode signing inputs. Defaults work when no certificate is
# configured (sign-windows prints a warning and exits 0); CI overrides
# these to point at the actual code-signing keypair.
#   SIGNTOOL                — path to signtool.exe (release CI runs on a
#                              Windows or wine'd host).
#   CODESIGN_CERT_PATH      — .pfx (or .p12) file with the signing key.
#   CODESIGN_CERT_PASS_FILE — file containing the .pfx password. Read
#                              with `cat` so the password never appears
#                              in `ps`/job logs.
#   CODESIGN_TIMESTAMP_URL  — RFC3161 timestamp authority. Default
#                              digicert.com; alternatives include
#                              http://timestamp.sectigo.com.
SIGNTOOL                ?= signtool.exe
CODESIGN_CERT_PATH      ?=
CODESIGN_CERT_PASS_FILE ?=
CODESIGN_TIMESTAMP_URL  ?= http://timestamp.digicert.com

.PHONY: build all-platforms clean test vet tidy upx sign-windows

build: ## Build the agent binary for $GOOS/$GOARCH.
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) \
		$(GO) build -trimpath -ldflags "$(LDFLAGS)" \
		-o $(BIN_DIR)/$(BIN_NAME)-$(GOOS)-$(GOARCH)$(EXT) ./cmd/ahv-remote-console-agent
	@cp $(BIN_DIR)/$(BIN_NAME)-$(GOOS)-$(GOARCH)$(EXT) $(BIN_DIR)/$(BIN_NAME)$(EXT)

# all-platforms iterates the TARGETS matrix and builds each in turn.
# Used by the release CI to produce the four binaries that
# "Install Agent" picks from based on the guest's reported OS+arch.
# After the cross-compiles complete, sign the Windows binaries with
# Authenticode so SmartScreen doesn't warn operators on first run.
all-platforms: ## Cross-compile every guest target and sign Windows binaries.
	@mkdir -p $(BIN_DIR)
	@for tgt in $(TARGETS); do \
	  os=$${tgt%/*}; arch=$${tgt#*/}; ext=""; \
	  if [ "$$os" = "windows" ]; then ext=".exe"; fi; \
	  echo "→ $$os/$$arch"; \
	  ldf="$(LDFLAGS)"; \
	  if [ "$$os" = "windows" ]; then ldf="$$ldf -H windowsgui"; fi; \
	  CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch \
	    $(GO) build -trimpath -ldflags "$$ldf" \
	    -o $(BIN_DIR)/$(BIN_NAME)-$$os-$$arch$$ext ./cmd/ahv-remote-console-agent; \
	done
	@$(MAKE) sign-windows
	@ls -lh $(BIN_DIR)

# sign-windows applies Authenticode signatures to every windows-* binary
# in $(BIN_DIR). Runs only when both SIGNTOOL is available on PATH AND
# a certificate is configured; otherwise prints a clear warning and
# exits 0 so local dev builds (where no cert is configured) succeed.
#
# CI sets CODESIGN_CERT_PATH + CODESIGN_CERT_PASS_FILE from a sealed
# secret store; the password is read from a file rather than passed on
# the command line so it doesn't leak into `ps` output or build logs.
#
# Closes the "Windows SmartScreen warns operators on first run" gap
# described in the task brief — once a signed binary is shipped, the
# initial-install warning disappears for any operator on a domain-
# joined Windows host that trusts the issuing CA.
sign-windows: ## Authenticode-sign windows-* binaries when a cert is configured.
	@set -e; \
	if ! command -v "$(SIGNTOOL)" >/dev/null 2>&1; then \
	  echo "[sign-windows] $(SIGNTOOL) not found on PATH; Authenticode certificate not configured; bridge will trigger SmartScreen warnings"; \
	  exit 0; \
	fi; \
	if [ -z "$(CODESIGN_CERT_PATH)" ] || [ -z "$(CODESIGN_CERT_PASS_FILE)" ]; then \
	  echo "[sign-windows] Authenticode certificate not configured; bridge will trigger SmartScreen warnings"; \
	  echo "[sign-windows] set CODESIGN_CERT_PATH and CODESIGN_CERT_PASS_FILE to enable signing"; \
	  exit 0; \
	fi; \
	if [ ! -f "$(CODESIGN_CERT_PATH)" ]; then \
	  echo "[sign-windows] CODESIGN_CERT_PATH=$(CODESIGN_CERT_PATH) does not exist"; \
	  exit 1; \
	fi; \
	if [ ! -f "$(CODESIGN_CERT_PASS_FILE)" ]; then \
	  echo "[sign-windows] CODESIGN_CERT_PASS_FILE=$(CODESIGN_CERT_PASS_FILE) does not exist"; \
	  exit 1; \
	fi; \
	pass=$$(cat "$(CODESIGN_CERT_PASS_FILE)"); \
	for f in $(BIN_DIR)/$(BIN_NAME)-windows-*.exe; do \
	  [ -f "$$f" ] || continue; \
	  echo "[sign-windows] signing $$f"; \
	  "$(SIGNTOOL)" sign \
	    /f "$(CODESIGN_CERT_PATH)" \
	    /p "$$pass" \
	    /tr "$(CODESIGN_TIMESTAMP_URL)" \
	    /td sha256 \
	    /fd sha256 \
	    "$$f"; \
	done

# UPX-compress every binary in $(BIN_DIR). Optional; CI runs it on
# release builds to ship ~3MB binaries instead of ~10MB. Requires
# the upx binary on PATH.
upx:
	@command -v upx >/dev/null 2>&1 || { echo "upx not installed"; exit 1; }
	@for f in $(BIN_DIR)/$(BIN_NAME)-*; do \
	  echo "compressing $$f"; \
	  upx --best --lzma "$$f" || true; \
	done

test:
	$(GO) test -race -cover ./...

vet:
	$(GO) vet ./...

tidy:
	$(GO) mod tidy

clean:
	rm -rf $(BIN_DIR)
