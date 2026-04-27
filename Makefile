APP_NAME    := mautrix-imessage-v2
CMD_PKG     := mautrix-imessage
BUNDLE_ID   := com.lrhodin.mautrix-imessage
VERSION     := 0.1.0
COMMIT      := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_TIME  := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
DATA_DIR    ?= $(HOME)/.local/share/mautrix-imessage
UNAME_S     := $(shell uname -s)

RUST_LIB    := librustpushgo.a
RUST_SRC    := $(shell find pkg/rustpushgo/src -name '*.rs' -o -name '*.m' -o -name '*.h' 2>/dev/null) \
               pkg/rustpushgo/build.rs \
               $(shell find nac-validation/src -name '*.rs' -o -name '*.m' -o -name '*.h' 2>/dev/null) \
               nac-validation/build.rs
# rustpush source root (default: upstream worktree checkout).
# Override at invocation time, e.g.:
#   make RUSTPUSH_DIR=rustpush-master build
RUSTPUSH_DIR ?= third_party/rustpush-upstream
# rustpush source strategy:
#   fork (default): clone cameronaaron/rustpush which has all bridge-compat
#                   fixes already applied — no runtime patching needed.
#   upstream:       clone raw OpenBubbles/rustpush (no patches; for testing only).
# Pinned OpenBubbles/rustpush commit. Edit this file manually to bump, then
# test locally before committing. The Makefile reads the SHA on build and
# checks out that exact commit — no auto-bump, no branch drift, no fork refs.
RUSTPUSH_PIN_FILE := third_party/rustpush-upstream.sha
RUSTPUSH_PIN      := $(shell cat $(RUSTPUSH_PIN_FILE) 2>/dev/null)
RUSTPUSH_SRC:= $(shell find $(RUSTPUSH_DIR)/src $(RUSTPUSH_DIR)/apple-private-apis $(RUSTPUSH_DIR)/open-absinthe/src -name '*.rs' -o -name '*.s' 2>/dev/null) $(wildcard $(RUSTPUSH_DIR)/open-absinthe/build.rs)
CARGO_FILES := $(shell find . -name 'Cargo.toml' -o -name 'Cargo.lock' 2>/dev/null | grep -v target)
GO_SRC      := $(shell find pkg/ cmd/ -name '*.go' 2>/dev/null)

BBCTL       := bbctl
LDFLAGS     := -X main.Tag=$(VERSION) -X main.Commit=$(COMMIT) -X main.BuildTime=$(BUILD_TIME)

# Track the git commit so ldflags changes trigger a Go rebuild.
# .build-commit is updated whenever HEAD changes.
COMMIT_FILE := .build-commit
PREV_COMMIT := $(shell cat $(COMMIT_FILE) 2>/dev/null)
ifneq ($(COMMIT),$(PREV_COMMIT))
  $(shell echo $(COMMIT) > $(COMMIT_FILE))
endif

.PHONY: build clean install install-beeper uninstall reset rust bindings check-deps check-deps-linux

# ===========================================================================
# Path validation – spaces in the working directory break CGO linker flags
# and #cgo LDFLAGS ${SRCDIR} expansion. Detect early with a clear message.
# ===========================================================================
ifneq ($(word 2,$(CURDIR)),)
  $(error The project path "$(CURDIR)" contains spaces. CGO and the linker cannot handle spaces in library paths. Please clone or move the project to a path without spaces, e.g.: /home/$$USER/imessage)
endif

# ===========================================================================
# Platform detection
# ===========================================================================

ifeq ($(UNAME_S),Darwin)
  # macOS paths and settings
  export PATH := /opt/homebrew/bin:/opt/homebrew/sbin:$(PATH)
  APP_BUNDLE  := $(APP_NAME).app
  BINARY      := $(APP_BUNDLE)/Contents/MacOS/$(APP_NAME)
  INFO_PLIST  := $(APP_BUNDLE)/Contents/Info.plist
  CGO_CFLAGS  := -I/opt/homebrew/include
  CGO_LDFLAGS := -L/opt/homebrew/lib -L$(CURDIR)
  CARGO_ENV   := MACOSX_DEPLOYMENT_TARGET=13.0
else
  # Linux: include Go and Rust installed by bootstrap
  export PATH := /usr/local/go/bin:$(HOME)/.cargo/bin:$(PATH)
  BINARY      := $(APP_NAME)
  CGO_CFLAGS  :=
  CGO_LDFLAGS := -L$(CURDIR)
  CARGO_ENV   :=
endif

# ===========================================================================
# Dependency checks
# ===========================================================================

# macOS: auto-install via Homebrew
check-deps:
ifeq ($(UNAME_S),Darwin)
	@if ! command -v brew >/dev/null 2>&1; then \
		echo "Installing Homebrew..."; \
		NONINTERACTIVE=1 /bin/bash -c "$$(curl -fsSL https://raw.githubusercontent.com/Homebrew/install/HEAD/install.sh)"; \
		eval "$$(/opt/homebrew/bin/brew shellenv)"; \
	fi; \
	missing=""; \
	command -v go >/dev/null 2>&1    || missing="$$missing go"; \
	command -v cargo >/dev/null 2>&1 || missing="$$missing rust"; \
	command -v protoc >/dev/null 2>&1|| missing="$$missing protobuf"; \
	command -v tmux >/dev/null 2>&1  || missing="$$missing tmux"; \
	[ -f /opt/homebrew/include/olm/olm.h ] || [ -f /usr/local/include/olm/olm.h ] || missing="$$missing libolm"; \
	pkg-config --exists libheif 2>/dev/null || missing="$$missing libheif"; \
	if [ -n "$$missing" ]; then \
		echo "Installing dependencies:$$missing"; \
		brew install $$missing; \
	fi
else
	@scripts/bootstrap-linux.sh
endif

# ===========================================================================
# Rust static library
# ===========================================================================

# On Linux, enable hardware-key feature (open-absinthe x86 NAC emulator).
# On macOS, native AAAbsintheContext is used — no unicorn/cmake needed.
ifeq ($(UNAME_S),Darwin)
  CARGO_FEATURES :=
else
  CARGO_FEATURES := --features hardware-key
endif

UPSTREAM_REPO := https://github.com/OpenBubbles/rustpush.git
FAIRPLAY_CERTS := 4056631661436364584235346952193 \
                  4056631661436364584235346952194 \
                  4056631661436364584235346952195 \
                  4056631661436364584235346952196 \
                  4056631661436364584235346952197 \
                  4056631661436364584235346952198 \
                  4056631661436364584235346952199 \
                  4056631661436364584235346952200 \
                  4056631661436364584235346952201 \
                  4056631661436364584235346952208

.PHONY: ensure-rustpush-source

# Prepare rustpush sources the same way upstream CI does: checkout with
# submodules present and fake FairPlay certs available for build-time signing.
ensure-rustpush-source:
	@if [ "$(RUSTPUSH_DIR)" = "rustpush-master" ]; then \
		if [ ! -f rustpush-master/apple-private-apis/icloud-auth/Cargo.toml ]; then \
			if [ -f $(FALLBACK_RUSTPUSH_DIR)/apple-private-apis/icloud-auth/Cargo.toml ]; then \
				echo "Hydrating rustpush-master/apple-private-apis from $(FALLBACK_RUSTPUSH_DIR)..."; \
				mkdir -p rustpush-master/apple-private-apis; \
				rm -rf rustpush-master/apple-private-apis/icloud-auth rustpush-master/apple-private-apis/omnisette; \
				cp -R $(FALLBACK_RUSTPUSH_DIR)/apple-private-apis/icloud-auth rustpush-master/apple-private-apis/; \
				cp -R $(FALLBACK_RUSTPUSH_DIR)/apple-private-apis/omnisette rustpush-master/apple-private-apis/; \
			else \
				echo "error: rustpush-master is missing apple-private-apis and fallback $(FALLBACK_RUSTPUSH_DIR) copy is unavailable" >&2; exit 1; \
			fi; \
		fi; \
		if [ ! -f rustpush-master/open-absinthe/Cargo.toml ]; then \
			if [ -f $(FALLBACK_RUSTPUSH_DIR)/open-absinthe/Cargo.toml ]; then \
				echo "Hydrating rustpush-master/open-absinthe from $(FALLBACK_RUSTPUSH_DIR)..."; \
				rm -rf rustpush-master/open-absinthe; \
				cp -R $(FALLBACK_RUSTPUSH_DIR)/open-absinthe rustpush-master/; \
			else \
				echo "error: rustpush-master is missing open-absinthe and fallback $(FALLBACK_RUSTPUSH_DIR) copy is unavailable" >&2; exit 1; \
			fi; \
		fi; \
		if [ ! -d rustpush-master/certs/fairplay ]; then \
			echo "Generating rustpush-master FairPlay cert stubs..."; \
			mkdir -p rustpush-master/certs/fairplay; \
			for name in $(FAIRPLAY_CERTS); do \
				cp rustpush-master/certs/legacy-fairplay/fairplay.crt rustpush-master/certs/fairplay/$$name.crt; \
				cp rustpush-master/certs/legacy-fairplay/fairplay.pem rustpush-master/certs/fairplay/$$name.pem; \
			done; \
		fi; \
	elif [ "$(RUSTPUSH_DIR)" = "third_party/rustpush-upstream" ]; then \
		if [ -z "$(RUSTPUSH_PIN)" ]; then \
			echo "error: $(RUSTPUSH_PIN_FILE) is missing or empty — required to pin rustpush SHA" >&2; exit 1; \
		fi; \
		export GIT_CONFIG_COUNT=1; \
		export GIT_CONFIG_KEY_0="url.https://github.com/.insteadOf"; \
		export GIT_CONFIG_VALUE_0="git@github.com:"; \
		if [ ! -d third_party/rustpush-upstream/.git ]; then \
			echo "Cloning OpenBubbles/rustpush at pinned SHA $(RUSTPUSH_PIN)..."; \
			mkdir -p third_party; \
			git clone $(UPSTREAM_REPO) third_party/rustpush-upstream || exit 1; \
			git -C third_party/rustpush-upstream checkout $(RUSTPUSH_PIN) || exit 1; \
			git -C third_party/rustpush-upstream submodule sync --recursive || exit 1; \
			git -C third_party/rustpush-upstream submodule update --init --recursive || exit 1; \
		fi; \
		current=$$(git -C third_party/rustpush-upstream rev-parse HEAD 2>/dev/null || echo none); \
		if [ "$$current" != "$(RUSTPUSH_PIN)" ]; then \
			echo "Checking out pinned rustpush SHA $(RUSTPUSH_PIN) (was $$current)..."; \
			git -C third_party/rustpush-upstream remote set-url origin $(UPSTREAM_REPO); \
			echo "Discarding any local mods to third_party/rustpush-upstream before checkout..."; \
			git -C third_party/rustpush-upstream reset --hard HEAD || exit 1; \
			git -C third_party/rustpush-upstream clean -fd || exit 1; \
			git -C third_party/rustpush-upstream fetch --all --tags --prune || exit 1; \
			git -C third_party/rustpush-upstream checkout $(RUSTPUSH_PIN) || { echo "error: failed to checkout pinned SHA $(RUSTPUSH_PIN)" >&2; exit 1; }; \
			git -C third_party/rustpush-upstream submodule sync --recursive || exit 1; \
			git -C third_party/rustpush-upstream submodule update --init --recursive || exit 1; \
		fi; \
		if [ ! -d third_party/rustpush-upstream/certs/fairplay ]; then \
			echo "Generating FairPlay cert stubs..."; \
			mkdir -p third_party/rustpush-upstream/certs/fairplay; \
			for name in $(FAIRPLAY_CERTS); do \
				cp third_party/rustpush-upstream/certs/legacy-fairplay/fairplay.crt \
				   third_party/rustpush-upstream/certs/fairplay/$$name.crt; \
				cp third_party/rustpush-upstream/certs/legacy-fairplay/fairplay.pem \
				   third_party/rustpush-upstream/certs/fairplay/$$name.pem; \
			done; \
		fi; \
		if [ -d rustpush/open-absinthe ] && [ -f rustpush/open-absinthe/Cargo.toml ]; then \
			if ! diff -rq rustpush/open-absinthe third_party/rustpush-upstream/open-absinthe >/dev/null 2>&1; then \
				echo "Overlaying our open-absinthe (native NAC wiring) onto $(RUSTPUSH_DIR)/open-absinthe..."; \
				rm -rf third_party/rustpush-upstream/open-absinthe; \
				cp -Rp rustpush/open-absinthe third_party/rustpush-upstream/open-absinthe; \
			fi; \
		fi; \
		if grep -q '^mod activation;' $(RUSTPUSH_DIR)/src/lib.rs 2>/dev/null; then \
			echo "Making rustpush activation module public (needed by RelayOSConfig)..."; \
			sed -i.bak 's/^mod activation;/pub mod activation;/' $(RUSTPUSH_DIR)/src/lib.rs && rm -f $(RUSTPUSH_DIR)/src/lib.rs.bak; \
		fi; \
		if grep -q '^mod ids;' $(RUSTPUSH_DIR)/src/lib.rs 2>/dev/null; then \
			echo "Making rustpush ids module public (needed by FT RespondedElsewhere overlay)..."; \
			sed -i.bak 's/^mod ids;/pub mod ids;/' $(RUSTPUSH_DIR)/src/lib.rs && rm -f $(RUSTPUSH_DIR)/src/lib.rs.bak; \
		fi; \
		if grep -q '^    token: String,$$' $(RUSTPUSH_DIR)/apple-private-apis/icloud-auth/src/client.rs 2>/dev/null; then \
			echo "Making FetchedToken.token pub (needed to replay persisted PET on session restore)..."; \
			sed -i.bak 's/^    token: String,$$/    pub token: String,/' $(RUSTPUSH_DIR)/apple-private-apis/icloud-auth/src/client.rs && rm -f $(RUSTPUSH_DIR)/apple-private-apis/icloud-auth/src/client.rs.bak; \
		fi; \
		if grep -q '^    expiration: SystemTime,$$' $(RUSTPUSH_DIR)/apple-private-apis/icloud-auth/src/client.rs 2>/dev/null; then \
			echo "Making FetchedToken.expiration pub..."; \
			sed -i.bak 's/^    expiration: SystemTime,$$/    pub expiration: SystemTime,/' $(RUSTPUSH_DIR)/apple-private-apis/icloud-auth/src/client.rs && rm -f $(RUSTPUSH_DIR)/apple-private-apis/icloud-auth/src/client.rs.bak; \
		fi; \
		if grep -q '^pub use client::{AppleAccount, LoginState,' $(RUSTPUSH_DIR)/apple-private-apis/icloud-auth/src/lib.rs 2>/dev/null; then \
			echo "Re-exporting FetchedToken from icloud_auth crate root..."; \
			sed -i.bak 's/^pub use client::{AppleAccount, LoginState,/pub use client::{AppleAccount, FetchedToken, LoginState,/' $(RUSTPUSH_DIR)/apple-private-apis/icloud-auth/src/lib.rs && rm -f $(RUSTPUSH_DIR)/apple-private-apis/icloud-auth/src/lib.rs.bak; \
		fi; \
	fi

# `ensure-rustpush-source` is an order-only prereq (the `|` separator):
# it runs before the recipe when needed, but its phony "always-dirty"
# timestamp doesn't force $(RUST_LIB) to rebuild on every `make` invocation.
# Only actual Rust source changes / Cargo.toml changes should trigger a
# rebuild; the pinned SHA + submodule setup is idempotent once done.
$(RUST_LIB): $(RUST_SRC) $(RUSTPUSH_SRC) $(CARGO_FILES) | ensure-rustpush-source
	cd pkg/rustpushgo && $(CARGO_ENV) cargo build --release $(CARGO_FEATURES)
	cp pkg/rustpushgo/target/release/librustpushgo.a .

rust: $(RUST_LIB)

# ===========================================================================
# Go bindings
# ===========================================================================

bindings: $(RUST_LIB)
	cd pkg/rustpushgo && uniffi-bindgen-go target/release/librustpushgo.a --library --out-dir ..
	python3 scripts/patch_bindings.py

# ===========================================================================
# Build
# ===========================================================================

ifeq ($(UNAME_S),Darwin)
build: check-deps $(RUST_LIB) $(BINARY) $(BBCTL)
	codesign --force --deep --sign - $(APP_BUNDLE)
	@echo "Built $(APP_BUNDLE) + $(BBCTL) ($(VERSION)-$(COMMIT))"

$(BINARY): $(GO_SRC) $(shell find . -name '*.m' -o -name '*.h' 2>/dev/null | grep -v target) go.mod go.sum $(RUST_LIB) $(COMMIT_FILE)
	@mkdir -p $(APP_BUNDLE)/Contents/MacOS
	@cp Info.plist $(INFO_PLIST)
	CGO_CFLAGS="$(CGO_CFLAGS)" CGO_LDFLAGS="$(CGO_LDFLAGS)" \
		go build -ldflags '$(LDFLAGS)' -o $(BINARY) ./cmd/$(CMD_PKG)/
else
build: check-deps $(RUST_LIB) $(BINARY) $(BBCTL)
	@echo "Built $(BINARY) + $(BBCTL) ($(VERSION)-$(COMMIT))"

$(BINARY): $(GO_SRC) go.mod go.sum $(RUST_LIB) $(COMMIT_FILE)
	CGO_CFLAGS="$(CGO_CFLAGS)" CGO_LDFLAGS="$(CGO_LDFLAGS)" \
		go build -ldflags '$(LDFLAGS)' -o $(BINARY) ./cmd/$(CMD_PKG)/
endif

$(BBCTL): $(GO_SRC) go.mod go.sum
	go build -o $(BBCTL) ./cmd/bbctl/

# ===========================================================================
# Install / uninstall (macOS)
# ===========================================================================

install: build
ifeq ($(UNAME_S),Darwin)
	@scripts/install.sh "$(BINARY)" "$(DATA_DIR)" "$(BUNDLE_ID)"
else
	@scripts/install-linux.sh "$(BINARY)" "$(DATA_DIR)"
endif

install-beeper: build
ifeq ($(UNAME_S),Darwin)
	@scripts/install-beeper.sh "$(BINARY)" "$(DATA_DIR)" "$(BUNDLE_ID)" "$(CURDIR)/$(BBCTL)"
else
	@scripts/install-beeper-linux.sh "$(BINARY)" "$(DATA_DIR)" "$(CURDIR)/$(BBCTL)"
endif

reset:
ifeq ($(UNAME_S),Darwin)
	@scripts/reset-bridge.sh "$(BUNDLE_ID)"
else
	@scripts/reset-bridge.sh
endif

uninstall:
ifeq ($(UNAME_S),Darwin)
	-launchctl unload ~/Library/LaunchAgents/$(BUNDLE_ID).plist 2>/dev/null
	rm -f ~/Library/LaunchAgents/$(BUNDLE_ID).plist
	@echo "LaunchAgent removed. App bundle at $(APP_BUNDLE) left in place."
else
	@echo "On Linux, stop the service and remove the binary manually."
endif

# ===========================================================================
# Extract-key (hardware key extraction tool)
# ===========================================================================
# extract-key uses CGO with Objective-C and macOS frameworks (Foundation,
# IOKit, DiskArbitration), so it can only be compiled on macOS.
# It has its own go.mod (Go 1.20) so it can build on macOS 10.13 High Sierra.
#
# On older Macs without Go installed, use the self-contained build script:
#   cd tools/extract-key && ./build.sh

.PHONY: extract-key

ifeq ($(UNAME_S),Darwin)
extract-key:
	cd tools/extract-key && go build -trimpath -o ../../extract-key .
else
extract-key:
	@echo "Error: extract-key must be built on macOS." >&2
	@echo "It uses Objective-C and macOS frameworks (IOKit, Foundation, DiskArbitration)." >&2
	@echo "" >&2
	@echo "On the target Mac (including High Sierra 10.13+):" >&2
	@echo "  cd tools/extract-key && ./build.sh" >&2
	@exit 1
endif

clean:
ifeq ($(UNAME_S),Darwin)
	rm -rf $(APP_NAME).app
endif
	rm -f $(APP_NAME) $(BBCTL) $(RUST_LIB) extract-key tools/extract-key/extract-key
	cd pkg/rustpushgo && cargo clean 2>/dev/null || true
