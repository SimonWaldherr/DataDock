# DataDock Makefile
# Works on Linux, macOS and Windows (Git Bash / MSYS).
#
# Precedence for the HTTP port on every run/menu path:
#   1. ADDR=host:port or PORT=xxxx given on the `make` command line (always wins)
#   2. the port saved in DataDock's own settings from a previous run
#   3. an auto-detected free port between 8000 and 8100

SHELL       := bash
.SHELLFLAGS := -eu -o pipefail -c
.ONESHELL:

.PHONY: all build run run-memory test menu tui help findport clean

GO     ?= go
APP    ?= datadock
DB     ?= datadock.db
TENANT ?= default
PORT   ?=
ADDR   ?=

GOEXE := $(shell $(GO) env GOEXE)
BIN   := $(APP)$(GOEXE)

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
	echo "  make menu         interactive launcher (default when running plain 'make')"
	echo "  make run          start the server   [DB=..] [TENANT=..] [PORT=..|ADDR=host:port]"
	echo "  make run-memory   start the server with an in-memory database"
	echo "  make build        build ./$(BIN)"
	echo "  make test         run the Go test suite"
	echo "  make findport     print a free TCP port between 8000-8100"
	echo "  make clean        remove the built binary"
	echo ""
	echo "PORT/ADDR given on the command line always take precedence over any"
	echo "port saved in DataDock's settings or the auto-detected free port."

build:
	@echo "==> building $(BIN)"
	$(GO) build -o $(BIN) .

test:
	@echo "==> running tests"
	$(GO) test ./...

clean:
	@rm -f $(BIN)
	echo "==> removed $(BIN)"

findport:
	@$(GO) run . -find-free-port

run:
	@echo "==> starting DataDock ($(RUN_ARGS))"
	$(GO) run . $(RUN_ARGS)

run-memory:
	@mem_args="-db :memory: -tenant $(TENANT)"
	if [ -n "$(strip $(ADDR))" ]; then mem_args="$$mem_args -addr $(ADDR)"; \
	elif [ -n "$(strip $(PORT))" ]; then mem_args="$$mem_args -port $(PORT)"; fi
	echo "==> starting DataDock ($$mem_args)"
	$(GO) run . $$mem_args

# Interactive TUI launcher. Prompts for database, tenant and port, but any
# PORT/ADDR passed on the `make` command line skips the port prompt and is
# used as-is (it always takes precedence).
tui: menu

menu:
	@clear 2>/dev/null || true
	suggestion=$$($(GO) run . -find-free-port 2>/dev/null || echo 8080)
	while true; do
	  echo "========================================"
	  echo "   DataDock - Interactive Launcher"
	  echo "========================================"
	  echo " 1) Run server"
	  echo " 2) Run server (in-memory database)"
	  echo " 3) Build binary"
	  echo " 4) Run tests"
	  echo " 5) Suggest a free port (8000-8100)"
	  echo " 0) Exit"
	  echo "----------------------------------------"
	  read -r -p "Choice [1]: " choice
	  choice=$${choice:-1}
	  case "$$choice" in
	    1)
	      read -r -p "Database file [$(DB)] (':memory:' for none): " db_in
	      db_in=$${db_in:-$(DB)}
	      read -r -p "Tenant [$(TENANT)]: " tenant_in
	      tenant_in=$${tenant_in:-$(TENANT)}
	      if [ -n "$(strip $(ADDR))" ]; then
	        echo "--> using ADDR=$(ADDR) from the command line (overrides saved/auto port)"
	        $(GO) run . -db "$$db_in" -tenant "$$tenant_in" -addr "$(ADDR)"
	      elif [ -n "$(strip $(PORT))" ]; then
	        echo "--> using PORT=$(PORT) from the command line (overrides saved/auto port)"
	        $(GO) run . -db "$$db_in" -tenant "$$tenant_in" -port "$(PORT)"
	      else
	        read -r -p "Port (empty = reuse saved port / auto-detect, suggestion $$suggestion): " port_in
	        if [ -n "$$port_in" ]; then
	          $(GO) run . -db "$$db_in" -tenant "$$tenant_in" -port "$$port_in"
	        else
	          $(GO) run . -db "$$db_in" -tenant "$$tenant_in"
	        fi
	      fi
	      break
	      ;;
	    2)
	      read -r -p "Tenant [$(TENANT)]: " tenant_in
	      tenant_in=$${tenant_in:-$(TENANT)}
	      if [ -n "$(strip $(ADDR))" ]; then
	        echo "--> using ADDR=$(ADDR) from the command line (overrides saved/auto port)"
	        $(GO) run . -db :memory: -tenant "$$tenant_in" -addr "$(ADDR)"
	      elif [ -n "$(strip $(PORT))" ]; then
	        echo "--> using PORT=$(PORT) from the command line (overrides saved/auto port)"
	        $(GO) run . -db :memory: -tenant "$$tenant_in" -port "$(PORT)"
	      else
	        read -r -p "Port (empty = reuse saved port / auto-detect, suggestion $$suggestion): " port_in
	        if [ -n "$$port_in" ]; then
	          $(GO) run . -db :memory: -tenant "$$tenant_in" -port "$$port_in"
	        else
	          $(GO) run . -db :memory: -tenant "$$tenant_in"
	        fi
	      fi
	      break
	      ;;
	    3)
	      $(GO) build -o $(BIN) .
	      echo "--> built $(BIN)"
	      ;;
	    4)
	      $(GO) test ./...
	      ;;
	    5)
	      echo "--> suggested free port: $$($(GO) run . -find-free-port)"
	      ;;
	    0)
	      break
	      ;;
	    *)
	      echo "invalid choice: $$choice"
	      ;;
	  esac
	  echo ""
	done
