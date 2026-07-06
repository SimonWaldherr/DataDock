#!/usr/bin/env bash
set -euo pipefail

go_cmd=${GO_CMD:-go}
bin_name=${BIN_NAME:-datadock}
db_default=${DB:-datadock.db}
tenant_default=${TENANT:-default}
port_override=${PORT:-}
addr_override=${ADDR:-}
use_tty=0
if [ -r /dev/tty ] && [ -w /dev/tty ]; then
  use_tty=1
fi

ask() {
  local prompt=$1
  local var_name=$2
  if [ "$use_tty" -eq 1 ]; then
    printf "%s" "$prompt" > /dev/tty
    IFS= read -r "$var_name" < /dev/tty
  else
    printf "%s" "$prompt"
    IFS= read -r "$var_name"
  fi
}

run_datadock() {
  echo "--> starting DataDock ($*)"
  "$go_cmd" run . "$@"
}

format_sources() {
  echo "--> formatting Go sources"
  "$go_cmd" fmt ./...
  if [ -f package.json ]; then
    echo "--> running npm formatter"
    npm run format
  else
    echo "--> no package.json; skipping npm run format"
  fi
}

clear 2>/dev/null || true
suggestion=$("$go_cmd" run . -find-free-port 2>/dev/null || echo 8080)

while true; do
  echo "========================================"
  echo "   DataDock - Interactive Launcher"
  echo "========================================"
  echo " 1) Run server"
  echo " 2) Run server (in-memory database)"
  echo " 3) Build binary"
  echo " 4) Run tests"
  echo " 5) Format code"
  echo " 6) Suggest a free port (8000-8100)"
  echo " 0) Exit"
  echo "----------------------------------------"
  ask "Choice [1]: " choice
  choice=${choice:-1}

  case "$choice" in
    1)
      ask "Database file [$db_default] (':memory:' for none): " db_in
      db_in=${db_in:-$db_default}
      ask "Tenant [$tenant_default]: " tenant_in
      tenant_in=${tenant_in:-$tenant_default}
      if [ -n "$addr_override" ]; then
        echo "--> using ADDR=$addr_override from the command line (overrides saved/auto port)"
        run_datadock -db "$db_in" -tenant "$tenant_in" -addr "$addr_override"
      elif [ -n "$port_override" ]; then
        echo "--> using PORT=$port_override from the command line (overrides saved/auto port)"
        run_datadock -db "$db_in" -tenant "$tenant_in" -port "$port_override"
      else
        ask "Port (empty = reuse saved port / auto-detect, suggestion $suggestion): " port_in
        if [ -n "$port_in" ]; then
          run_datadock -db "$db_in" -tenant "$tenant_in" -port "$port_in"
        else
          run_datadock -db "$db_in" -tenant "$tenant_in"
        fi
      fi
      break
      ;;
    2)
      ask "Tenant [$tenant_default]: " tenant_in
      tenant_in=${tenant_in:-$tenant_default}
      if [ -n "$addr_override" ]; then
        echo "--> using ADDR=$addr_override from the command line (overrides saved/auto port)"
        run_datadock -db :memory: -tenant "$tenant_in" -addr "$addr_override"
      elif [ -n "$port_override" ]; then
        echo "--> using PORT=$port_override from the command line (overrides saved/auto port)"
        run_datadock -db :memory: -tenant "$tenant_in" -port "$port_override"
      else
        ask "Port (empty = reuse saved port / auto-detect, suggestion $suggestion): " port_in
        if [ -n "$port_in" ]; then
          run_datadock -db :memory: -tenant "$tenant_in" -port "$port_in"
        else
          run_datadock -db :memory: -tenant "$tenant_in"
        fi
      fi
      break
      ;;
    3)
      "$go_cmd" build -o "$bin_name" .
      echo "--> built $bin_name"
      ;;
    4)
      "$go_cmd" test ./...
      ;;
    5)
      format_sources
      ;;
    6)
      echo "--> suggested free port: $("$go_cmd" run . -find-free-port)"
      ;;
    0)
      break
      ;;
    *)
      echo "invalid choice: $choice"
      ;;
  esac
  echo ""
done
