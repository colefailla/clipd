BINARY  := clipd
PKG     := ./cmd/clipd

# Version metadata is injected at link time so a released binary can identify
# itself. A plain `go build` still works and reports the "dev" placeholders.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS := -s -w \
	-X main.version=$(VERSION) \
	-X main.commit=$(COMMIT) \
	-X main.date=$(DATE)

# -trimpath keeps local filesystem paths out of the binary, which makes builds
# reproducible across machines.
GOFLAGS := -trimpath -ldflags '$(LDFLAGS)'

PREFIX ?= /usr/local

# sudo is used only when the install directory is not writable as-is, so
# installing into a user-owned prefix (PREFIX=$HOME/.local) needs no password.
# Missing directories fall back to testing the nearest existing ancestor, so
# a user prefix that has not been created yet does not force sudo (which
# would leave a root-owned directory inside the user's home).
SUDO := $(shell p="$(PREFIX)/bin"; while [ ! -e "$$p" ]; do p=$$(dirname "$$p"); done; test -w "$$p" || echo sudo)

# Must match launchagent.Label. Changing one without the other leaves an
# orphaned service loaded under the old name.
LABEL := com.clipd.agent

PLATFORMS := darwin/arm64 darwin/amd64 linux/amd64 linux/arm64

.DEFAULT_GOAL := build

.PHONY: help
help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'

.PHONY: build
build: ## Build for the host platform into bin/
	@mkdir -p bin
	go build $(GOFLAGS) -o bin/$(BINARY) $(PKG)

.PHONY: dist
dist: ## Cross-compile release binaries into dist/
	@mkdir -p dist
	@for platform in $(PLATFORMS); do \
		os=$${platform%/*}; arch=$${platform#*/}; \
		echo "  building $$os/$$arch"; \
		GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 \
			go build $(GOFLAGS) -o dist/$(BINARY)_$${os}_$${arch} $(PKG) || exit 1; \
	done
	@ls -la dist/

.PHONY: test
test: ## Run the tests
	go test ./...

.PHONY: race
race: ## Run the tests under the race detector
	go test -race ./...

.PHONY: cover
cover: ## Run the tests and report coverage per package
	go test -cover ./...

.PHONY: vet
vet: ## Run go vet for the host and for linux
	go vet ./...
	GOOS=linux GOARCH=amd64 go vet ./...

.PHONY: fmt
fmt: ## Format the source
	gofmt -s -w .

.PHONY: fmt-check
fmt-check: ## Fail if any file needs formatting
	@unformatted=$$(gofmt -s -l .); \
	if [ -n "$$unformatted" ]; then \
		echo "not gofmt-clean:"; echo "$$unformatted"; exit 1; \
	fi

.PHONY: check
check: fmt-check vet race ## Everything CI should run

.PHONY: install
install: build ## Install into $(PREFIX)/bin (may need sudo)
	$(SUDO) install -d $(PREFIX)/bin
	$(SUDO) install -m 0755 bin/$(BINARY) $(PREFIX)/bin/$(BINARY)
	@echo "installed $(PREFIX)/bin/$(BINARY)"

.PHONY: reinstall
reinstall: install ## macOS dev loop: build, install, restart the daemon, show status
	@[ "$$(uname -s)" = "Darwin" ] || { \
		echo "reinstall restarts a launchd agent and so is macOS-only;"; \
		echo "on Linux there is no daemon and 'make install' is the whole job."; \
		exit 1; }
	@# Replacing the binary on disk does not touch the process already running
	@# it: launchd keeps executing the old inode until the service is
	@# restarted. Skipping this step is the reason a rebuild appears to have
	@# no effect, so it belongs in the same command as the install.
	@if out=$$(launchctl kickstart -k gui/$$(id -u)/$(LABEL) 2>&1); then \
		echo "restarted $(LABEL)"; \
	else \
		echo "WARNING: $(LABEL) was not restarted: $$out"; \
		echo "         the new binary is installed but the daemon is still running the old one."; \
		echo "         if the LaunchAgent is not set up yet, run: $(PREFIX)/bin/$(BINARY) install"; \
	fi
	@echo
	@$(PREFIX)/bin/$(BINARY) version | head -3
	@echo
	@$(PREFIX)/bin/$(BINARY) status

.PHONY: uninstall
uninstall: ## Remove the installed binary
	rm -f $(PREFIX)/bin/$(BINARY)

.PHONY: clean
clean: ## Remove build output
	rm -rf bin dist
