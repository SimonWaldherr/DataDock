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

.PHONY: all build run run-memory test vet staticcheck vulncheck check fmt format menu tui help findport clean

GO     ?= go
NPM    ?= npm
APP    ?= datadock
DB     ?= datadock.db
TENANT ?= default
PORT   ?=
ADDR   ?=

GOEXE = $(shell $(GO) env GOEXE)
BIN   = $(APP)$(GOEXE)

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
	@echo "  make build        build ./$(APP)"
	@echo "  make test         run the Go test suite"
	@echo "  make vet          run go vet"
	@echo "  make staticcheck  run staticcheck when installed"
	@echo "  make vulncheck    run govulncheck when installed"
	@echo "  make check        run test, vet, staticcheck and govulncheck"
	@echo "  make fmt          run go fmt and npm run format when package.json exists"
	@echo "  make findport     print a free TCP port between 8000-8100"
	@echo "  make clean        remove the built binary"
	@echo ""
	@echo "PORT/ADDR given on the command line always take precedence over any"
	@echo "port saved in DataDock's settings or the auto-detected free port."

build:
	@echo "==> building $(BIN)"
	$(GO) build -o $(BIN) .

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

check: test vet staticcheck vulncheck

fmt format:
	@echo "==> formatting Go sources"
	$(GO) fmt ./...
	@if [ -f package.json ]; then \
		echo "==> running npm formatter"; \
		$(NPM) run format; \
	else \
		echo "==> no package.json; skipping npm run format"; \
	fi

clean:
	@rm -f $(BIN)
	echo "==> removed $(BIN)"

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
