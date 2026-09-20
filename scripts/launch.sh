#!/usr/bin/env bash
# One-command launcher: checks prerequisites, builds and starts every service,
# waits until the stack is healthy, then opens the web UI in the browser.
#
# Usage: scripts/launch.sh [--no-open] [--smoke] [--timeout SECONDS]
#   --no-open   do not open the browser (just print the URL)
#   --smoke     also send the same prompt twice and confirm miss -> hit
#   --timeout   seconds to wait for the stack to become healthy (default 240)
set -euo pipefail

URL="http://localhost:8080/"
OPEN_BROWSER=1
SMOKE=0
TIMEOUT=240

while [ $# -gt 0 ]; do
  case "$1" in
    --no-open) OPEN_BROWSER=0 ;;
    --smoke) SMOKE=1 ;;
    --timeout) TIMEOUT="${2:?--timeout needs a value}"; shift ;;
    -h|--help) sed -n '2,8p' "$0"; exit 0 ;;
    *) echo "unknown option: $1" >&2; exit 2 ;;
  esac
  shift
done

step() { printf '\n==> %s\n' "$*"; }
fail() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

cd "$(dirname "$0")/.."

step "Checking prerequisites"
command -v docker >/dev/null 2>&1 || fail "docker is not installed. See the README 'Prerequisites' section."
docker compose version >/dev/null 2>&1 || fail "'docker compose' plugin is missing. See the README 'Prerequisites' section."
command -v curl >/dev/null 2>&1 || fail "curl is not installed."
docker info >/dev/null 2>&1 || fail "Docker is not running. Start Docker Desktop (or the docker service), wait for 'Engine running', then re-run."
echo "docker, docker compose and curl are available; the Docker engine is running."

step "Preparing configuration"
if [ -f .env ]; then
  echo ".env already exists - leaving it untouched."
else
  cp .env.example .env
  echo "Created .env from .env.example (defaults work out of the box)."
fi

step "Building and starting services (docker compose up --build -d)"
docker compose up --build -d

step "Waiting for the stack to become healthy (up to ${TIMEOUT}s)"
# The embedding service loads a sentence-transformers model on boot, so the
# orchestrator can take a while to report healthy.
deadline=$((SECONDS + TIMEOUT))
until curl -fs "http://localhost:8080/health" 2>/dev/null | grep -q '"status" *: *"ok"'; do
  if [ "$SECONDS" -ge "$deadline" ]; then
    echo "Stack did not become healthy in ${TIMEOUT}s. Container status:" >&2
    docker compose ps >&2
    echo "Recent logs:" >&2
    docker compose logs --tail=30 >&2
    fail "timed out waiting for http://localhost:8080/health"
  fi
  printf '.'
  sleep 3
done
echo
for port in 8001 8002; do
  printf 'port %s: ' "$port"
  curl -fs "http://localhost:${port}/health" || fail "service on port ${port} is not healthy"
  echo
done
printf 'orchestrator: '
curl -fs "http://localhost:8080/health"
echo

if [ "$SMOKE" -eq 1 ]; then
  step "Smoke test: same prompt twice (expect miss, then hit)"
  query() {
    curl -fs localhost:8080/query -H "Content-Type: application/json" \
      -d '{"prompt":"What is virtual memory?"}'
  }
  first="$(query)"
  echo "first : $first"
  echo "$first" | grep -q '"cache_hit" *: *false' || fail "first query was not a cache miss (cache may already hold this prompt; run 'curl -X POST localhost:8080/flush' and retry)"
  # The cache update is applied asynchronously through the queue.
  sleep 2
  second="$(query)"
  echo "second: $second"
  echo "$second" | grep -q '"cache_hit" *: *true' || fail "second query was not a cache hit"
  echo "Smoke test passed."
fi

open_url() {
  if grep -qi microsoft /proc/version 2>/dev/null; then
    if command -v wslview >/dev/null 2>&1; then wslview "$1"; return; fi
    if command -v explorer.exe >/dev/null 2>&1; then explorer.exe "$1" || true; return; fi
    if command -v powershell.exe >/dev/null 2>&1; then powershell.exe -NoProfile -Command "Start-Process '$1'"; return; fi
  fi
  if command -v open >/dev/null 2>&1; then open "$1"; return; fi
  if command -v xdg-open >/dev/null 2>&1; then xdg-open "$1" >/dev/null 2>&1 & return; fi
  return 1
}

step "Ready"
echo "Web UI: ${URL}"
if [ "$OPEN_BROWSER" -eq 1 ]; then
  open_url "$URL" || echo "Could not open a browser automatically - open ${URL} yourself."
fi
echo "Stop everything with: docker compose down"
