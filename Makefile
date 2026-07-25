# DataDock Makefile
# Works on Linux, macOS and Windows (Git Bash / MSYS).
#
# Precedence for the HTTP port on every run/menu path:
#   1. ADDR=host:port or PORT=xxxx given on the `make` command line (always wins)
#   2. the port saved in DataDock's own settings from a previous run
#   3. an auto-detected free port between 8000 and 8100

SHELL       := bash
.SHELLFLAGS := -eu -o pipefail -c
# Do not rely on .ONESHELL here: macOS commonly ships GNU Make 3.81, which
# ignores it. Multi-line shell logic must be continued explicitly or moved into
# scripts/make-menu.sh.

.PHONY: all build build-check run run-memory test vet staticcheck vulncheck check ci fmt format fmt-check format-check menu tui help findport clean dist docker

GO     ?= go
NPM    ?= npm
APP    ?= datadock
DB     ?= datadock.db
TENANT ?= default
PORT   ?=
ADDR   ?=

GOEXE = $(shell $(GO) env GOEXE)
BIN   = $(APP)$(GOEXE)

# VERSION is stamped into the binary via -ldflags so `datadock` and /version
# report an identifiable build. Defaults to `git describe` (nearest tag +
# commit + dirty flag); override with `make build VERSION=1.2.3`.
VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
GO_LDFLAGS ?= -X main.version=$(VERSION)

RUN_ARGS := -db $(DB) -tenant $(TENANT)
ifneq ($(strip $(ADDR)),)
RUN_ARGS += -addr $(ADDR)
else ifneq ($(strip $(PORT)),)
RUN_ARGS += -port $(PORT)
endif

.DEFAULT_GOAL := menu

all: build

help:
	@echo "DataDock make targets:"
	@echo "  make menu         interactive launcher (default when running plain 'make')"
	@echo "  make run          start the server   [DB=..] [TENANT=..] [PORT=..|ADDR=host:port]"
	@echo "  make run-memory   start the server with an in-memory database"
	@echo "  make build        build ./$(APP) (version-stamped)"
	@echo "  make dist         cross-compile release binaries into dist/"
	@echo "  make docker       build the container image ($(APP):$(VERSION))"
	@echo "  make build-check  compile every Go package without writing a binary"
	@echo "  make test         run the Go test suite"
	@echo "  make vet          run go vet"
	@echo "  make staticcheck  run staticcheck when installed"
	@echo "  make vulncheck    run govulncheck when installed"
	@echo "  make check        run fmt-check, test, vet, build, staticcheck and govulncheck"
	@echo "  make ci           run the local CI subset without optional external linters"
	@echo "  make fmt          run go fmt and npm run format when package.json exists"
	@echo "  make fmt-check    check gofmt and npm asset formatting without modifying files"
	@echo "  make findport     print a free TCP port between 8000-8100"
	@echo "  make clean        remove the built binary"
	@echo ""
	@echo "PORT/ADDR given on the command line always take precedence over any"
	@echo "port saved in DataDock's settings or the auto-detected free port."

build:
	@echo "==> building $(BIN) ($(VERSION))"
	$(GO) build -ldflags "$(GO_LDFLAGS)" -o $(BIN) .

build-check:
	@echo "==> checking package builds"
	$(GO) build ./...

test:
	@echo "==> running tests"
	$(GO) test ./...

vet:
	@echo "==> running go vet"
	$(GO) vet ./...

staticcheck:
	@echo "==> running staticcheck"
	@if command -v staticcheck >/dev/null 2>&1; then \
		staticcheck ./...; \
	else \
		echo "staticcheck not installed; install with: go install honnef.co/go/tools/cmd/staticcheck@latest"; \
		exit 1; \
	fi

vulncheck:
	@echo "==> running govulncheck"
	@if command -v govulncheck >/dev/null 2>&1; then \
		govulncheck ./...; \
	else \
		echo "govulncheck not installed; install with: go install golang.org/x/vuln/cmd/govulncheck@latest"; \
		exit 1; \
	fi

check: fmt-check test vet build-check staticcheck vulncheck

ci: fmt-check test vet build-check

fmt format:
	@echo "==> formatting Go sources"
	$(GO) fmt ./...
	@if [ -f package.json ]; then \
		echo "==> running npm formatter"; \
		$(NPM) run format; \
	else \
		echo "==> no package.json; skipping npm run format"; \
	fi

fmt-check format-check:
	@echo "==> checking Go formatting"
	@files="$$(gofmt -l $$(find . -name '*.go' -not -path './.git/*'))"; \
	if [ -n "$$files" ]; then \
		echo "$$files"; \
		echo "Go files need gofmt; run: make fmt"; \
		exit 1; \
	fi
	@if [ -f package.json ]; then \
		echo "==> checking npm formatter"; \
		$(NPM) run format:check; \
	else \
		echo "==> no package.json; skipping npm format check"; \
	fi

clean:
	@rm -f $(BIN)
	@rm -rf $(DIST_DIR)
	echo "==> removed $(BIN) and $(DIST_DIR)"

# Cross-compile release binaries into dist/. Pure Go (CGO disabled) so no C
# toolchain is needed for any target. goreleaser does this in CI (see
# .goreleaser.yaml); this target is for local/manual dist builds.
DIST_DIR     ?= dist
DIST_TARGETS ?= linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64

dist:
	@echo "==> cross-building $(APP) $(VERSION) -> $(DIST_DIR)/"
	@rm -rf $(DIST_DIR)
	@for target in $(DIST_TARGETS); do \
		os=$${target%/*}; arch=$${target#*/}; ext=""; \
		if [ "$$os" = "windows" ]; then ext=".exe"; fi; \
		out="$(DIST_DIR)/$(APP)_$${os}_$${arch}$$ext"; \
		echo "  $$out"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch $(GO) build -trimpath -ldflags "$(GO_LDFLAGS)" -o "$$out" . || exit 1; \
	done
	@echo "==> done ($(DIST_DIR)/)"

# Build the container image, tagged with VERSION and latest.
docker:
	@echo "==> docker build $(APP):$(VERSION)"
	docker build --build-arg VERSION=$(VERSION) -t $(APP):$(VERSION) -t $(APP):latest .

findport:
	@$(GO) run . -find-free-port

run:
	@echo "==> starting DataDock ($(RUN_ARGS))"
	$(GO) run . $(RUN_ARGS)

run-memory:
	@mem_args="-db :memory: -tenant $(TENANT)"; \
	if [ -n "$(strip $(ADDR))" ]; then mem_args="$$mem_args -addr $(ADDR)"; \
	elif [ -n "$(strip $(PORT))" ]; then mem_args="$$mem_args -port $(PORT)"; fi; \
	echo "==> starting DataDock ($$mem_args)"; \
	$(GO) run . $$mem_args

# Interactive TUI launcher. Prompts for database, tenant and port, but any
# PORT/ADDR passed on the `make` command line skips the port prompt and is
# used as-is (it always takes precedence).
tui: menu

menu:
	@GO_CMD="$(GO)" BIN_NAME="$(BIN)" DB="$(DB)" TENANT="$(TENANT)" PORT="$(strip $(PORT))" ADDR="$(strip $(ADDR))" bash scripts/make-menu.sh
