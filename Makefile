# AHV Remote Console — top-level Makefile.
# Coordinates Go runtime, AHV plugin, and Electron build.

# Cross-platform shell selection. On Unix `/usr/bin/env bash` is the
# canonical way to find bash. On Windows that path does not exist
# and make fails with "the system cannot find the path specified"
# before any recipe runs — so we pin SHELL to a bare `bash` and rely
# on PATH (Git for Windows / WSL / MSYS2 all install bash.exe on
# PATH). Recipes use bash-only syntax (set -e/pipefail, $(…) subshells,
# for/if loops, here-docs), so a real bash is required on every host.
ifeq ($(OS),Windows_NT)
HOST_OS := windows
SHELL := bash
else
HOST_OS := $(shell uname -s | tr A-Z a-z)
SHELL := /usr/bin/env bash
endif
.SHELLFLAGS := -eo pipefail -c

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "0.0.0-dev")
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
# Default to the HEAD commit date (not wall clock) so a rebuild of the same
# commit embeds the same Date ldflag and produces byte-identical binaries —
# the agent ISO is content-addressed in the image catalog (SHA-256 in the
# name), so wall-clock stamps re-uploaded a "new" identical image on every
# build. Override DATE explicitly to stamp something else.
DATE    ?= $(shell git log -1 --format=%cI 2>/dev/null || date -u +%Y-%m-%dT%H:%M:%SZ)

export AHV_REMOTE_CONSOLE_VERSION := $(VERSION)
export AHV_REMOTE_CONSOLE_COMMIT  := $(COMMIT)
export AHV_REMOTE_CONSOLE_DATE    := $(DATE)

# -----------------------------------------------------------------------------
# Build
# -----------------------------------------------------------------------------

.PHONY: all build runtime plugins desktop dev icons clean test lint typecheck package install ci agent-bins agent-pkgs iso

# Where the runtime expects to find signed-manifest agent binaries.
# Override via the AHV_REMOTE_CONSOLE_AGENT_BIN_DIR env var that the
# runtime reads at startup. Keep the default relative path so a `make
# agent-bins && make runtime && ./apps/runtime/...` flow Just Works from
# the repo root.
AGENT_BIN_DIR ?= build/agent

all: build

build: runtime plugins desktop ## Build everything for the host platform.

runtime: ## Build the Go runtime.
	$(MAKE) -C apps/runtime build

plugins: ## Build all plugins.
	$(MAKE) -C plugins/ahv build

desktop: ## Build the Electron desktop shell.
	pnpm --filter '@ahv-remote-console/*' build

# Cross-compile the in-guest agent daemon for the supported targets
# (linux-amd64, windows-amd64 — AHV guests are x86-64 only) and stage
# them under $(AGENT_BIN_DIR) with the names the isobuilder expects:
# ahv-remote-console-agent-<os>-<arch>[.exe].
#
# The agent Makefile produces binaries named
# `ahv-remote-console-agent-<os>-<arch>[.exe]`; we stage them here so
# the isobuilder (cmd/isobuilder) picks them up into the ISO.
#
# AHV_REMOTE_CONSOLE_AGENT_BIN_DIR (env, read by the runtime at
# startup) selects which directory the runtime serves from. Default:
# $(AGENT_BIN_DIR) i.e. `build/agent/` at the repo root.
agent-bins: ## Cross-compile + stage the in-guest agent binaries for the ISO.
	@$(MAKE) -C apps/ahv-remote-console-agent all-platforms
	@mkdir -p $(AGENT_BIN_DIR)
	@for tgt in linux/amd64 windows/amd64; do \
	  os=$${tgt%/*}; arch=$${tgt#*/}; ext=""; \
	  if [ "$$os" = "windows" ]; then ext=".exe"; fi; \
	  src=apps/ahv-remote-console-agent/bin/ahv-remote-console-agent-$$os-$$arch$$ext; \
	  dst=$(AGENT_BIN_DIR)/ahv-remote-console-agent-$$os-$$arch$$ext; \
	  if [ -f "$$src" ]; then \
	    cp -f "$$src" "$$dst"; \
	    echo "→ $$dst"; \
	  else \
	    echo "skip: missing $$src" >&2; \
	  fi; \
	done
	@ls -lh $(AGENT_BIN_DIR)

# Package the Linux agent binary as distro packages (.rpm + .deb) so a
# desktop-Linux guest can install it by double-clicking in Files (GNOME
# Software/Discover → Install → "Agent Setup"). Pure-Go packaging
# (cmd/agentpkg, nfpm) — no rpm/dpkg toolchain needed on the build host.
# The launcher icon comes from the generated icon set; the file rule
# below regenerates it only when missing, so Go-only flows don't need
# pnpm once icons exist.
AGENT_ICON ?= build/icons/256x256.png

$(AGENT_ICON):
	pnpm build:icons

# Package metadata timestamp = the HEAD commit date, so identical inputs
# produce byte-identical packages across rebuilds. The catalog image the
# ISO uploads to is deduped by the SHA-256 embedded in its name — wall-
# clock package timestamps would make every rebuild look like a new image.
SOURCE_DATE ?= $(shell git log -1 --format=%ct 2>/dev/null || echo 0)

agent-pkgs: agent-bins $(AGENT_ICON) ## Package the Linux agent as .rpm + .deb for the ISO.
	go run ./cmd/agentpkg \
	  -bin $(AGENT_BIN_DIR)/ahv-remote-console-agent-linux-amd64 \
	  -icon $(AGENT_ICON) \
	  -out $(AGENT_BIN_DIR) \
	  -version $(VERSION) \
	  -mtime $(SOURCE_DATE)

# Assemble the agent ISO from the staged binaries in $(AGENT_BIN_DIR).
# Output is a single hybrid ISO9660+Joliet image at build/agent.iso,
# ready for upload to the Prism Image Service. The runtime attaches
# this as a CD-ROM to the target VM; the operator runs the platform
# installer (the .exe on Windows; the .rpm/.deb or linux/install.sh on
# Linux). See the `cmd/isobuilder` package for the assembly logic.
#
# Depends on `agent-pkgs` (which depends on `agent-bins`) so the .iso is
# always built from fresh binaries AND fresh packages — running `make iso`
# alone can never ship a stale or missing artifact.
AGENT_ISO ?= build/agent.iso

iso: agent-pkgs ## Assemble the agent ISO ready for Prism Image Service upload.
	@mkdir -p $(dir $(AGENT_ISO))
	go run ./cmd/isobuilder \
	  -agent-dir $(AGENT_BIN_DIR) \
	  -out $(AGENT_ISO) \
	  -version $(VERSION) \
	  -mtime $(SOURCE_DATE)
	@echo ""
	@echo "ISO ready: $(AGENT_ISO)"
	@ls -lh $(AGENT_ISO) | awk '{print "  size: "$$5}'
	@if command -v shasum >/dev/null 2>&1; then \
	  shasum -a 256 $(AGENT_ISO) | awk '{print "  sha256: "$$1}'; \
	elif command -v sha256sum >/dev/null 2>&1; then \
	  sha256sum $(AGENT_ISO) | awk '{print "  sha256: "$$1}'; \
	fi

icons: ## Generate all app icon assets (PNGs, .icns, .ico, modes, tray) from the brand spark.
	pnpm build:icons

dev: icons ## Run everything in dev mode (regenerates icons first so the dock/window icon is current).
	node scripts/dev.mjs

clean: ## Remove build artifacts.
	$(MAKE) -C apps/runtime clean
	$(MAKE) -C plugins/ahv clean
	pnpm -r clean
	rm -rf $(AGENT_BIN_DIR)

# -----------------------------------------------------------------------------
# Quality gates
# -----------------------------------------------------------------------------

test: test-go test-ts ## Run all tests.

test-go:
	go test -race -cover ./...

test-ts:
	pnpm -r test

lint:
	$(MAKE) -C apps/runtime lint
	$(MAKE) -C plugins/ahv lint
	pnpm lint

typecheck:
	pnpm typecheck

# -----------------------------------------------------------------------------
# Packaging
# -----------------------------------------------------------------------------

# Stage runtime binaries where electron-builder expects them.
# electron-builder.yml extraResources uses build/runtime/${os}-${arch}/
# where ${os} ∈ {mac,win,linux} and ${arch} ∈ {arm64,x64}.
# Translates Go's GOOS/GOARCH naming on the way.
#
# Two invariants this recipe must hold (both were violated once and shipped
# a week-stale runtime inside an otherwise-fresh DMG):
#   1. The staged file is RENAMED to the canonical `ahv-remote-console-runtime`
#      — runtime-manager.ts resolveBinary() spawns that exact name, so copying
#      the GOOS/GOARCH-suffixed build next to a stale canonical ships the
#      stale one.
#   2. macOS packaging targets BOTH arches (mac.target.arch: [arm64, x64]),
#      so on a darwin host both are cross-built (CGO_ENABLED=0) and staged;
#      the dest dir is wiped first so nothing stale can ride along.
# The host arch is built LAST: apps/runtime's build target refreshes the
# canonical bin/ahv-remote-console-runtime convenience copy on every build,
# and dev mode (resolveBinary) spawns that copy — it must end up host-arch.
# (No plugin .so/.dylib/.dll staging: the AHV plugin is statically linked
# into the runtime — see the extraResources note in electron-builder.yml.)
stage-runtime: ## Build + stage runtime binaries into build/runtime/.
	@host_os=$$(go env GOOS); host_arch=$$(go env GOARCH); \
	if [ "$$host_os" = darwin ]; then \
	  other=$$([ "$$host_arch" = arm64 ] && echo amd64 || echo arm64); \
	  archs="$$other $$host_arch"; \
	else archs=$$host_arch; fi; \
	ext=''; [ "$$host_os" = windows ] && ext='.exe'; \
	eb_os=$$(echo $$host_os | sed 's/darwin/mac/;s/windows/win/'); \
	for goarch in $$archs; do \
	  $(MAKE) -C apps/runtime build GOOS=$$host_os GOARCH=$$goarch; \
	  eb_arch=$$(echo $$goarch | sed 's/amd64/x64/;s/386/ia32/'); \
	  dest="build/runtime/$$eb_os-$$eb_arch"; \
	  rm -rf "$$dest"; mkdir -p "$$dest"; \
	  cp "apps/runtime/bin/ahv-remote-console-runtime-$$host_os-$$goarch$$ext" \
	     "$$dest/ahv-remote-console-runtime$$ext"; \
	  echo "Staged $$dest/ahv-remote-console-runtime$$ext"; \
	done

package: build stage-runtime ## Produce installer artifacts for current platform.
	pnpm --filter @ahv-remote-console/desktop package

package-win:
	pnpm --filter @ahv-remote-console/desktop package:win

package-linux:
	pnpm --filter @ahv-remote-console/desktop package:linux

# Signed macOS package (production artifact, ELEC-02 entitlements). Refuses
# to build without a real signing identity — an ad-hoc app carrying the prod
# entitlements aborts at launch in dyld (hardened runtime's library
# validation can never pass without a Team ID on the Electron Framework; see
# the 1.0.278 crash). Gate-verified after packaging.
package-mac: require-mac-identity
	pnpm --filter @ahv-remote-console/desktop package:mac
	$(MAKE) verify-mac-release

# Local UNSIGNED smoke build — NOT shippable, artifact named DEV-UNSIGNED.
# Deterministically ad-hoc (auto-discovery off) with the dev entitlements
# profile (disable-library-validation), host arch only.
package-mac-dev:
	pnpm --filter @ahv-remote-console/desktop package:mac:dev

# A signing identity is present when the keychain holds a Developer ID
# Application cert OR CI supplies one via CSC_LINK/CSC_NAME.
require-mac-identity:
	@if [ -n "$$CSC_LINK" ] || [ -n "$$CSC_NAME" ]; then exit 0; fi; \
	security find-identity -v -p codesigning 2>/dev/null | grep -q "Developer ID Application" || { \
	  echo "no 'Developer ID Application' identity in the keychain and no CSC_LINK/CSC_NAME set."; \
	  echo "A build signed ad-hoc with the prod entitlements aborts at launch (library"; \
	  echo "validation, ELEC-02). Use 'make package-mac-dev' for an unsigned local build."; \
	  exit 1; }

prod: install build agent-bins iso stage-runtime ## Full production build: deps → runtime → plugins → agent → ISO → icons → package.
	pnpm build:icons
	pnpm --filter @ahv-remote-console/desktop package
	@echo ""
	@echo "=== Production build complete ==="
	@echo "Artifacts in apps/desktop/release/"
	@ls -lh apps/desktop/release/ 2>/dev/null || echo "(no artifacts — electron-builder may need platform-specific flags)"

prod-mac: install build agent-bins iso stage-runtime require-mac-identity ## Production build targeting macOS.
	pnpm build:icons
	pnpm --filter @ahv-remote-console/desktop package:mac
	$(MAKE) verify-mac-release
	@echo "=== macOS build complete ==="
	@ls -lh apps/desktop/release/*.dmg 2>/dev/null

# Release gate: a macOS artifact that is ad-hoc, missing hardened runtime, or
# carrying the dev entitlement must never leave the pipeline. Runs
# automatically at the end of prod-mac; run standalone before shipping any
# previously-built artifact.
verify-mac-release: ## Assert the packaged mac app is release-grade (Developer-ID signed, hardened, prod entitlements).
	@app=$$(ls -d apps/desktop/release/mac*/*.app 2>/dev/null | head -1); \
	[ -n "$$app" ] || { echo "verify-mac-release: no .app under apps/desktop/release/"; exit 1; }; \
	echo "verify-mac-release: $$app"; \
	codesign --verify --deep --strict "$$app" || { echo "FAIL: codesign --verify --deep --strict"; exit 1; }; \
	info=$$(codesign -dv "$$app" 2>&1); \
	echo "$$info" | grep -Eq 'TeamIdentifier=[A-Z0-9]{10}' || { echo "FAIL: no Team ID (ad-hoc/self-signed) — not shippable"; exit 1; }; \
	echo "$$info" | grep -q 'runtime' || { echo "FAIL: hardened runtime missing"; exit 1; }; \
	if codesign -d --entitlements - "$$app" 2>/dev/null | grep -q 'disable-library-validation'; then \
	  echo "FAIL: dev entitlement (disable-library-validation) present in a release build"; exit 1; fi; \
	echo "OK: Developer-ID signed, hardened runtime, prod (ELEC-02) entitlements"

prod-win: install build agent-bins iso stage-runtime ## Production build targeting Windows (NSIS, x64+ia32).
	pnpm build:icons
	pnpm --filter @ahv-remote-console/desktop package:win
	@echo "=== Windows build complete ==="
	@ls -lh apps/desktop/release/*.exe 2>/dev/null

# Smoke build for local testing on macOS — Wine on macOS is 64-bit
# only (Catalina dropped 32-bit), so this skips the ia32 target.
# Full Windows installers (NSIS, x64+ia32) build on the CI
# runner-wine where wine32 is available via i386 multiarch.
prod-win-local: install build agent-bins iso stage-runtime ## Local NSIS x64-only Windows build (works on macOS Wine).
	pnpm build:icons
	cd apps/desktop && pnpm build && \
	  npx electron-builder --win --x64 -c.win.target=nsis -c.win.signAndEditExecutable=false --publish never
	@echo "=== Local Windows smoke build complete ==="
	@ls -lh apps/desktop/release/*.exe 2>/dev/null
	@echo ""
	@echo "Note: this is NSIS x64 only. For ia32, push to CI."

prod-linux: install build agent-bins iso stage-runtime ## Production build targeting Linux.
	pnpm build:icons
	pnpm --filter @ahv-remote-console/desktop package:linux
	@echo "=== Linux build complete ==="
	@ls -lh apps/desktop/release/*.AppImage apps/desktop/release/*.deb 2>/dev/null

# -----------------------------------------------------------------------------
# Bootstrap
# -----------------------------------------------------------------------------

install: ## Install JS and Go dependencies.
	pnpm install --frozen-lockfile || pnpm install
	cd apps/runtime && go mod download
	cd plugins/ahv && go mod download

ci: install lint typecheck test build ## Full CI pipeline.

help: ## Show this help.
	@awk 'BEGIN {FS=":.*?## "} /^[a-zA-Z_-]+:.*?## / {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

.DEFAULT_GOAL := help
