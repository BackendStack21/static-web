#!/usr/bin/env bash
# =============================================================================
# baremetal.sh — Bare-metal benchmark: static-web vs Bun
#
# Builds static-web from source, benchmarks three configurations on the same
# port one at a time, then prints a ranked comparison. No Docker.
#
# Configurations tested:
#   1. static-web  --benchmark-mode        (minimal handler, zero overhead)
#   2. static-web  --preload --gc-percent 400  (production optimised)
#   3. Bun         native static HTML server
#
# Usage:
#   ./benchmark/baremetal.sh [OPTIONS]
#
# Options:
#   -c <int>    Connections      (default: 50)
#   -n <int>    Total requests   (default: 100000)
#   -d <int>    Duration seconds — overrides -n when set
#   -p <int>    Port to use      (default: 8080)
#   -r <dir>    Root directory    (default: ./public)
#   -h          Show this help
#
# Requirements:
#   - go (to build static-web)
#   - bun (https://bun.sh)
#   - bombardier (https://github.com/codesenberg/bombardier)
# =============================================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
RESULTS_DIR="${SCRIPT_DIR}/results"

# ---------- defaults ---------------------------------------------------------
CONNECTIONS=50
REQUESTS=100000
DURATION=""
PORT=8080
ROOT_DIR="./public"
WARMUP_REQUESTS=50000
SETTLE_SECONDS=3      # pause between server start and warmup

# ---------- colours ----------------------------------------------------------
RED='\033[0;31m'; YELLOW='\033[1;33m'; GREEN='\033[0;32m'
CYAN='\033[0;36m'; BOLD='\033[1m'; DIM='\033[2m'; RESET='\033[0m'

# ---------- arg parse --------------------------------------------------------
usage() {
  grep '^#' "$0" | grep -v '^#!/' | sed 's/^# \{0,2\}//'
  exit 0
}

while getopts "c:n:d:p:r:h" opt; do
  case $opt in
    c) CONNECTIONS="$OPTARG" ;;
    n) REQUESTS="$OPTARG" ;;
    d) DURATION="$OPTARG" ;;
    p) PORT="$OPTARG" ;;
    r) ROOT_DIR="$OPTARG" ;;
    h) usage ;;
    *) echo "Unknown option -$OPTARG"; exit 1 ;;
  esac
done

# ---------- dependency checks ------------------------------------------------
check_deps() {
  local missing=""
  command -v go          >/dev/null 2>&1 || missing="$missing go"
  command -v bun         >/dev/null 2>&1 || missing="$missing bun"
  command -v bombardier  >/dev/null 2>&1 || missing="$missing bombardier"

  if [ -n "$missing" ]; then
    echo -e "${RED}Missing dependencies:${missing}${RESET}"
    echo ""
    echo "Install bombardier:  brew install bombardier"
    echo "Install bun:         curl -fsSL https://bun.sh/install | bash"
    exit 1
  fi
}

# ---------- helpers ----------------------------------------------------------
BIN="/tmp/static-web-baremetal-$$"
SERVER_PID=""

cleanup() {
  if [ -n "$SERVER_PID" ] && kill -0 "$SERVER_PID" 2>/dev/null; then
    kill "$SERVER_PID" 2>/dev/null
    wait "$SERVER_PID" 2>/dev/null || true
  fi
  rm -f "$BIN"
}
trap cleanup EXIT

wait_for_port() {
  local port=$1 max=15 i=0
  while ! curl -sf -o /dev/null "http://localhost:${port}/" 2>/dev/null; do
    sleep 0.5
    i=$((i + 1))
    if [ "$i" -ge "$max" ]; then
      echo -e " ${RED}TIMEOUT${RESET}"
      return 1
    fi
  done
}

kill_on_port() {
  local pids
  pids=$(lsof -ti :"$1" 2>/dev/null || true)
  if [ -n "$pids" ]; then
    echo "$pids" | xargs kill -9 2>/dev/null || true
    sleep 1
  fi
}

run_bombardier() {
  local url=$1
  if [ -n "$DURATION" ]; then
    bombardier -c "$CONNECTIONS" -d "${DURATION}s" -l --print r "$url" 2>/dev/null
  else
    bombardier -c "$CONNECTIONS" -n "$REQUESTS" -l --print r "$url" 2>/dev/null
  fi
}

parse_rps()  { awk '/Reqs\/sec/{print $2; exit}'; }
parse_p50()  { awk '/50\%/{print $2; exit}'; }
parse_p99()  { awk '/99\%/{print $2; exit}'; }
parse_tp()   { awk '/Throughput/{print $2; exit}'; }

# ---------- main -------------------------------------------------------------
main() {
  check_deps

  mkdir -p "$RESULTS_DIR"

  # Resolve root to absolute path
  local abs_root
  abs_root="$(cd "$PROJECT_ROOT" && cd "$ROOT_DIR" 2>/dev/null && pwd)" || {
    echo -e "${RED}Root directory not found: ${ROOT_DIR}${RESET}"
    exit 1
  }

  echo ""
  echo -e "${BOLD}╔════════════════════════════════════════════════════════════════════╗${RESET}"
  echo -e "${BOLD}║          Bare-Metal Benchmark: static-web vs Bun                  ║${RESET}"
  echo -e "${BOLD}╚════════════════════════════════════════════════════════════════════╝${RESET}"
  echo ""

  if [ -n "$DURATION" ]; then
    echo -e "  ${CYAN}Mode:         duration ${DURATION}s${RESET}"
  else
    echo -e "  ${CYAN}Mode:         ${REQUESTS} requests${RESET}"
  fi
  echo -e "  ${CYAN}Connections:  ${CONNECTIONS}${RESET}"
  echo -e "  ${CYAN}Warmup:       ${WARMUP_REQUESTS} requests${RESET}"
  echo -e "  ${CYAN}Port:         ${PORT}${RESET}"
  echo -e "  ${CYAN}Root:         ${abs_root}${RESET}"
  echo -e "  ${CYAN}Tool:         $(bombardier --version 2>&1 | head -1)${RESET}"
  echo -e "  ${CYAN}Go:           $(go version | awk '{print $3}')${RESET}"
  echo -e "  ${CYAN}Bun:          $(bun --version)${RESET}"
  echo -e "  ${CYAN}Date:         $(date -u '+%Y-%m-%d %H:%M:%S UTC')${RESET}"
  echo -e "  ${CYAN}OS/Arch:      $(uname -s)/$(uname -m)${RESET}"
  echo ""

  # ---- build static-web ----------------------------------------------------
  echo -e "${BOLD}→ Building static-web...${RESET}"
  (cd "$PROJECT_ROOT" && go build -ldflags="-s -w" -o "$BIN" ./cmd/static-web)
  echo -e "  ${GREEN}Built: ${BIN}${RESET}"
  echo ""

  # Make sure port is free
  kill_on_port "$PORT"

  local URL="http://localhost:${PORT}/index.html"

  # Result arrays (indexed: 0=benchmark-mode, 1=preload, 2=bun)
  local -a NAMES RPS_ARR P50_ARR P99_ARR TP_ARR
  NAMES=("static-web (benchmark-mode)" "static-web (preload+gc400)" "Bun")

  # ======================================================================
  #  Test 1: static-web  --benchmark-mode  (minimal handler)
  # ======================================================================
  echo -e "${BOLD}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${RESET}"
  echo -e "${BOLD}  [ static-web — benchmark mode (minimal handler) ]${RESET}"
  echo -e "${BOLD}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${RESET}"

  "$BIN" --benchmark-mode --port "$PORT" "$abs_root" &
  SERVER_PID=$!
  sleep "$SETTLE_SECONDS"
  wait_for_port "$PORT"
  echo -e "  ${GREEN}Server ready (PID ${SERVER_PID})${RESET}"

  echo -e "  ${DIM}Warming up (${WARMUP_REQUESTS} requests)...${RESET}"
  bombardier -c "$CONNECTIONS" -n "$WARMUP_REQUESTS" --print i "$URL" >/dev/null 2>&1
  echo -e "  ${DIM}Settle (${SETTLE_SECONDS}s)...${RESET}"
  sleep "$SETTLE_SECONDS"

  echo -e "  ${CYAN}Benchmarking...${RESET}"
  local raw
  raw=$(run_bombardier "$URL" | tee "${RESULTS_DIR}/baremetal-static-web-benchmark.txt")
  echo ""

  RPS_ARR[0]=$(echo "$raw" | parse_rps)
  P50_ARR[0]=$(echo "$raw" | parse_p50)
  P99_ARR[0]=$(echo "$raw" | parse_p99)
  TP_ARR[0]=$(echo "$raw"  | parse_tp)

  kill "$SERVER_PID" 2>/dev/null; wait "$SERVER_PID" 2>/dev/null || true
  SERVER_PID=""
  sleep 1
  kill_on_port "$PORT"

  # ======================================================================
  #  Test 2: static-web  --preload --gc-percent 400  (production mode)
  # ======================================================================
  echo -e "${BOLD}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${RESET}"
  echo -e "${BOLD}  [ static-web — production: --preload --gc-percent 400 ]${RESET}"
  echo -e "${BOLD}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${RESET}"

  "$BIN" --quiet --no-compress --preload --gc-percent 400 --port "$PORT" "$abs_root" &
  SERVER_PID=$!
  sleep "$SETTLE_SECONDS"
  wait_for_port "$PORT"
  echo -e "  ${GREEN}Server ready (PID ${SERVER_PID})${RESET}"

  echo -e "  ${DIM}Warming up (${WARMUP_REQUESTS} requests)...${RESET}"
  bombardier -c "$CONNECTIONS" -n "$WARMUP_REQUESTS" --print i "$URL" >/dev/null 2>&1
  echo -e "  ${DIM}Settle (${SETTLE_SECONDS}s)...${RESET}"
  sleep "$SETTLE_SECONDS"

  echo -e "  ${CYAN}Benchmarking...${RESET}"
  raw=$(run_bombardier "$URL" | tee "${RESULTS_DIR}/baremetal-static-web-preload.txt")
  echo ""

  RPS_ARR[1]=$(echo "$raw" | parse_rps)
  P50_ARR[1]=$(echo "$raw" | parse_p50)
  P99_ARR[1]=$(echo "$raw" | parse_p99)
  TP_ARR[1]=$(echo "$raw"  | parse_tp)

  kill "$SERVER_PID" 2>/dev/null; wait "$SERVER_PID" 2>/dev/null || true
  SERVER_PID=""
  sleep 1
  kill_on_port "$PORT"

  # ======================================================================
  #  Test 3: Bun static serve
  # ======================================================================
  echo -e "${BOLD}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${RESET}"
  echo -e "${BOLD}  [ Bun — native static HTML server ]${RESET}"
  echo -e "${BOLD}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${RESET}"

  (cd "$abs_root" && bun --port "$PORT" ./index.html) &
  SERVER_PID=$!
  sleep "$SETTLE_SECONDS"
  wait_for_port "$PORT"
  echo -e "  ${GREEN}Server ready (PID ${SERVER_PID})${RESET}"

  echo -e "  ${DIM}Warming up (${WARMUP_REQUESTS} requests)...${RESET}"
  bombardier -c "$CONNECTIONS" -n "$WARMUP_REQUESTS" --print i "$URL" >/dev/null 2>&1
  echo -e "  ${DIM}Settle (${SETTLE_SECONDS}s)...${RESET}"
  sleep "$SETTLE_SECONDS"

  echo -e "  ${CYAN}Benchmarking...${RESET}"
  raw=$(run_bombardier "$URL" | tee "${RESULTS_DIR}/baremetal-bun.txt")
  echo ""

  RPS_ARR[2]=$(echo "$raw" | parse_rps)
  P50_ARR[2]=$(echo "$raw" | parse_p50)
  P99_ARR[2]=$(echo "$raw" | parse_p99)
  TP_ARR[2]=$(echo "$raw"  | parse_tp)

  kill "$SERVER_PID" 2>/dev/null; wait "$SERVER_PID" 2>/dev/null || true
  SERVER_PID=""

  # ======================================================================
  #  Rank results (descending by RPS — insertion sort, 3 elements)
  # ======================================================================
  local -a SORTED_IDX=(0 1 2)
  local n=3 i=1
  while [ $i -lt $n ]; do
    local key_idx=${SORTED_IDX[$i]}
    local key_rps=${RPS_ARR[$key_idx]}
    local j=$((i - 1))
    while [ $j -ge 0 ]; do
      local cmp_idx=${SORTED_IDX[$j]}
      local cmp_rps=${RPS_ARR[$cmp_idx]}
      if awk "BEGIN{exit !($cmp_rps < $key_rps)}" 2>/dev/null; then
        SORTED_IDX[$((j + 1))]=${SORTED_IDX[$j]}
        j=$((j - 1))
      else
        break
      fi
    done
    SORTED_IDX[$((j + 1))]=$key_idx
    i=$((i + 1))
  done

  # ======================================================================
  #  Print results table
  # ======================================================================
  echo ""
  echo -e "${BOLD}╔════════════════════════════════════════════════════════════════════╗${RESET}"
  echo -e "${BOLD}║                    Bare-Metal Results                              ║${RESET}"
  echo -e "${BOLD}╠════════════════════════════════════════════════════════════════════╣${RESET}"
  printf "${BOLD}║  %-4s %-30s  %10s  %8s  %8s  ║${RESET}\n" \
    "#" "Server" "Req/sec" "p50" "p99"
  echo -e "${BOLD}╠════════════════════════════════════════════════════════════════════╣${RESET}"

  local rank=1
  for idx in "${SORTED_IDX[@]}"; do
    local colour medal
    if [ "$rank" -eq 1 ]; then
      colour="$GREEN"; medal="1st"
    elif [ "$rank" -eq 2 ]; then
      colour="$YELLOW"; medal="2nd"
    else
      colour="$RESET"; medal="3rd"
    fi

    printf "${colour}║  %-4s %-30s  %10s  %8s  %8s  ║${RESET}\n" \
      "$medal" "${NAMES[$idx]}" "${RPS_ARR[$idx]}" "${P50_ARR[$idx]}" "${P99_ARR[$idx]}"
    rank=$((rank + 1))
  done

  echo -e "${BOLD}╚════════════════════════════════════════════════════════════════════╝${RESET}"
  echo ""
  echo -e "  ${DIM}Throughput:${RESET}"
  echo -e "  ${DIM}  benchmark-mode  ${TP_ARR[0]}${RESET}"
  echo -e "  ${DIM}  preload+gc400   ${TP_ARR[1]}${RESET}"
  echo -e "  ${DIM}  Bun             ${TP_ARR[2]}${RESET}"
  echo -e "  ${DIM}Results saved to: ${RESULTS_DIR}/baremetal-*.txt${RESET}"
  echo ""
}

main "$@"
