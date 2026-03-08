#!/usr/bin/env bash
# =============================================================================
# bench.sh — Static file server benchmark suite
#
# Spins up static-web (default), static-web (preload), nginx, bun, and caddy
# via docker-compose, runs bombardier against each one serving /index.html,
# prints a ranked summary, then tears everything down.
#
# Usage:
#   ./benchmark/bench.sh [OPTIONS]
#
# Options:
#   -c <int>    Connections  (default: 50)
#   -n <int>    Total requests (default: 100000)
#   -d <int>    Duration in seconds — overrides -n when set
#   -k          Keep containers running after benchmark (default: tear down)
#   -h          Show this help
#
# Requirements:
#   - docker + docker compose
#   - bombardier (https://github.com/codesenberg/bombardier)
#     Install: brew install bombardier  OR  go install github.com/codesenberg/bombardier@latest
# =============================================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
COMPOSE_FILE="${SCRIPT_DIR}/docker-compose.benchmark.yml"
RESULTS_DIR="${SCRIPT_DIR}/results"

# ---------- defaults ---------------------------------------------------------
CONNECTIONS=50
REQUESTS=100000
DURATION=""       # empty = use -n mode; set seconds e.g. 30 to use -d mode
KEEP=false

# ---------- colours ----------------------------------------------------------
RED='\033[0;31m'; YELLOW='\033[1;33m'; GREEN='\033[0;32m'
CYAN='\033[0;36m'; BOLD='\033[1m'; RESET='\033[0m'

# ---------- arg parse --------------------------------------------------------
usage() {
  grep '^#' "$0" | grep -v '^#!/' | sed 's/^# \{0,2\}//'
  exit 0
}

while getopts "c:n:d:kh" opt; do
  case $opt in
    c) CONNECTIONS="$OPTARG" ;;
    n) REQUESTS="$OPTARG" ;;
    d) DURATION="$OPTARG" ;;
    k) KEEP=true ;;
    h) usage ;;
    *) echo "Unknown option -$OPTARG"; exit 1 ;;
  esac
done

# ---------- dependency checks ------------------------------------------------
check_deps() {
  local missing=""
  command -v docker     >/dev/null 2>&1 || missing="$missing docker"
  command -v bombardier >/dev/null 2>&1 || missing="$missing bombardier"

  if [ -n "$missing" ]; then
    echo -e "${RED}Missing dependencies:${missing}${RESET}"
    echo ""
    echo "Install bombardier:  brew install bombardier"
    echo "                  OR go install github.com/codesenberg/bombardier@latest"
    exit 1
  fi
}

# ---------- servers (parallel indexed arrays — bash 3 compatible) ------------
SERVER_NAMES=(  "static-web"                        "nginx"                        "bun"                        "caddy"                        "static-web-preload"                   )
SERVER_URLS=(   "http://localhost:8001/index.html"   "http://localhost:8002/index.html"   "http://localhost:8003/index.html"   "http://localhost:8004/index.html"   "http://localhost:8005/index.html" )
SERVER_COUNT=5

# ---------- helpers ----------------------------------------------------------
wait_for_server() {
  local name=$1
  local url=$2
  local max=30
  local i=0
  printf "  Waiting for %-22s" "${name}..."
  while ! curl -sf -o /dev/null "$url" 2>/dev/null; do
    sleep 1
    i=$((i + 1))
    if [ "$i" -ge "$max" ]; then
      echo -e " ${RED}TIMEOUT${RESET}"
      return 1
    fi
    printf "."
  done
  echo -e " ${GREEN}ready${RESET}"
}

run_bombardier() {
  local url=$1
  if [ -n "$DURATION" ]; then
    bombardier -c "$CONNECTIONS" -d "${DURATION}s" -l --print r "$url" 2>/dev/null
  else
    bombardier -c "$CONNECTIONS" -n "$REQUESTS"   -l --print r "$url" 2>/dev/null
  fi
}

# Extract Reqs/sec average from bombardier output
parse_rps() {
  awk '/Reqs\/sec/{print $2; exit}'
}

# Extract p50 latency
parse_p50() {
  awk '/50\%/{print $2; exit}'
}

# Extract p99 latency
parse_p99() {
  awk '/99\%/{print $2; exit}'
}

# ---------- main -------------------------------------------------------------
main() {
  check_deps

  mkdir -p "$RESULTS_DIR"

  echo ""
  echo -e "${BOLD}╔════════════════════════════════════════════════════════════════════╗${RESET}"
  echo -e "${BOLD}║            Static Web Server Benchmark Suite                      ║${RESET}"
  echo -e "${BOLD}╚════════════════════════════════════════════════════════════════════╝${RESET}"
  echo ""

  if [ -n "$DURATION" ]; then
    echo -e "  ${CYAN}Mode:        duration ${DURATION}s${RESET}"
  else
    echo -e "  ${CYAN}Mode:        ${REQUESTS} requests${RESET}"
  fi
  echo -e "  ${CYAN}Connections: ${CONNECTIONS}${RESET}"
  echo -e "  ${CYAN}Tool:        $(bombardier --version 2>&1 | head -1)${RESET}"
  echo -e "  ${CYAN}Date:        $(date -u '+%Y-%m-%d %H:%M:%S UTC')${RESET}"
  echo ""

  # ---- start containers -----------------------------------------------------
  echo -e "${BOLD}→ Starting containers...${RESET}"
  docker compose -f "$COMPOSE_FILE" up -d --build 2>&1 | \
    grep -E '(building|built|pulling|pulled|started|created|Built|Started|Created)' || true
  echo ""

  # ---- wait for readiness ---------------------------------------------------
  echo -e "${BOLD}→ Waiting for servers to be ready...${RESET}"
  i=0
  while [ $i -lt $SERVER_COUNT ]; do
    wait_for_server "${SERVER_NAMES[$i]}" "${SERVER_URLS[$i]}"
    i=$((i + 1))
  done
  echo ""

  # ---- warmup pass ----------------------------------------------------------
  echo -e "${BOLD}→ Warming up (10 000 requests each)...${RESET}"
  i=0
  while [ $i -lt $SERVER_COUNT ]; do
    printf "  %-22s" "${SERVER_NAMES[$i]}"
    bombardier -c "$CONNECTIONS" -n 10000 --print i "${SERVER_URLS[$i]}" >/dev/null 2>&1
    echo -e " ${GREEN}done${RESET}"
    i=$((i + 1))
  done
  echo ""

  # ---- benchmark each server ------------------------------------------------
  echo -e "${BOLD}→ Running benchmarks...${RESET}"
  echo ""

  # Parallel indexed result arrays
  RPS=()
  P50=()
  P99=()

  i=0
  while [ $i -lt $SERVER_COUNT ]; do
    name="${SERVER_NAMES[$i]}"
    url="${SERVER_URLS[$i]}"
    out_file="${RESULTS_DIR}/${name}.txt"

    echo -e "  ${BOLD}[ ${name} ]${RESET}  ${url}"
    echo -e "  ─────────────────────────────────────────────"

    raw=$(run_bombardier "$url" | tee "$out_file")

    rps=$(echo "$raw" | parse_rps)
    p50=$(echo "$raw" | parse_p50)
    p99=$(echo "$raw" | parse_p99)

    RPS[$i]="${rps:-0}"
    P50[$i]="${p50:-N/A}"
    P99[$i]="${p99:-N/A}"

    echo ""
    i=$((i + 1))
  done

  # ---- rank by req/s (simple insertion sort, bash 3 compatible) -------------
  # Build a sorted index array (descending by RPS)
  SORTED_IDX=()
  i=0
  while [ $i -lt $SERVER_COUNT ]; do
    SORTED_IDX[$i]=$i
    i=$((i + 1))
  done
  n=${#SORTED_IDX[@]}
  i=1
  while [ $i -lt $n ]; do
    key_idx=${SORTED_IDX[$i]}
    key_rps=${RPS[$key_idx]}
    j=$((i - 1))
    while [ $j -ge 0 ]; do
      cmp_idx=${SORTED_IDX[$j]}
      cmp_rps=${RPS[$cmp_idx]}
      # Compare floats via awk
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

  best_rps=${RPS[${SORTED_IDX[0]}]}

  echo -e "${BOLD}╔════════════════════════════════════════════════════════════════════╗${RESET}"
  echo -e "${BOLD}║                        Results Summary                             ║${RESET}"
  echo -e "${BOLD}╠════════════════════════════════════════════════════════════════════╣${RESET}"
  printf "${BOLD}║  %-4s %-22s  %12s  %10s  %10s  ║${RESET}\n" \
    "#" "Server" "Req/sec" "p50 lat" "p99 lat"
  echo -e "${BOLD}╠════════════════════════════════════════════════════════════════════╣${RESET}"

  rank=1
  for idx in "${SORTED_IDX[@]}"; do
    name="${SERVER_NAMES[$idx]}"
    rps="${RPS[$idx]}"
    p50="${P50[$idx]}"
    p99="${P99[$idx]}"

    if [ "$rank" -eq 1 ]; then
      colour="$GREEN"; medal="1st"
    elif [ "$rank" -eq 2 ]; then
      colour="$YELLOW"; medal="2nd"
    elif [ "$rank" -eq 3 ]; then
      colour="$YELLOW"; medal="3rd"
    else
      colour="$RESET"; medal="${rank}th"
    fi

    printf "${colour}║  %-4s %-22s  %12s  %10s  %10s  ║${RESET}\n" \
      "$medal" "$name" "$rps" "$p50" "$p99"
    rank=$((rank + 1))
  done

  echo -e "${BOLD}╚════════════════════════════════════════════════════════════════════╝${RESET}"
  echo ""
  echo -e "  Full results saved to: ${CYAN}${RESULTS_DIR}/${RESET}"
  echo ""

  # ---- teardown -------------------------------------------------------------
  if [ "$KEEP" = "false" ]; then
    echo -e "${BOLD}→ Tearing down containers...${RESET}"
    docker compose -f "$COMPOSE_FILE" down --remove-orphans 2>&1 | \
      grep -E '(Stopped|Removed|Removing|error)' || true
    echo ""
  else
    echo -e "  ${YELLOW}Containers left running (-k flag). Stop with:${RESET}"
    echo -e "  docker compose -f benchmark/docker-compose.benchmark.yml down"
    echo ""
  fi
}

main "$@"
