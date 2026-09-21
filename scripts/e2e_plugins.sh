#!/usr/bin/env bash
# End-to-end acceptance test of the plugin platform against the REAL stack.
#
# It starts the normal docker compose stack (plus the plugin controller and the
# restricted Docker socket proxy), then drives the admin API to install, verify,
# activate, upgrade, roll back and restore deterministic local test plugins (the
# SDK examples). No billed provider is ever called: the LLM is the built-in stub or
# an SDK example, and the embedding model is the local sentence-transformers one.
#
# Requirements: docker (with compose), git, go. The first run builds the stack's
# images (the embedding image is large) and pulls python:3.11-slim and golang:1.22-alpine.
#
#   scripts/e2e_plugins.sh            # run, then tear the stack down
#   KEEP=1 scripts/e2e_plugins.sh     # leave it running for inspection
set -euo pipefail
cd "$(dirname "$0")/.."

export PATH="$PATH:$HOME/go/bin"
PROJECT=llmcache-e2e
export E2E_PORT="${E2E_PORT:-18180}"
export E2E_REPOS="${E2E_REPOS:-/tmp/llmcache-e2e/repos}"
export ADMIN_TOKEN="e2e-admin-$(head -c 12 /dev/urandom | od -An -tx1 | tr -d ' \n')"
export PLUGIN_SECRET_KEY="$(head -c 32 /dev/urandom | base64)"
export PLUGIN_CONTROLLER_TOKEN="e2e-controller-$(head -c 12 /dev/urandom | od -An -tx1 | tr -d ' \n')"
COMPOSE=(docker compose -p "$PROJECT" -f docker-compose.yml -f docker-compose.e2e.yml --profile plugins)

step() { printf '\n==> %s\n' "$*"; }

cleanup() {
  if [ "${KEEP:-0}" = "1" ]; then
    echo "KEEP=1: stack left running on http://127.0.0.1:$E2E_PORT (project $PROJECT)."; return
  fi
  step "Tearing down"
  # Stop the stack first: the controller is attached to every plugin network, which
  # cannot be removed while it is.
  "${COMPOSE[@]}" down -v --remove-orphans >/dev/null 2>&1 || true
  comm -13 <(printf '%s\n' "$before_containers") <(docker ps -aq --filter 'label=llmcache.plugin=true' | sort) | xargs -r docker rm -f >/dev/null 2>&1 || true
  comm -13 <(printf '%s\n' "$before_networks") <(docker network ls -q --filter 'label=llmcache.plugin=true' | sort) | xargs -r docker network rm >/dev/null 2>&1 || true
  rm -rf "$E2E_REPOS"
}
# Remember which plugin containers/networks already exist so teardown removes only
# what THIS run created (other stacks on the machine may have their own).
before_containers="$(docker ps -aq --filter 'label=llmcache.plugin=true' | sort)"
before_networks="$(docker network ls -q --filter 'label=llmcache.plugin=true' | sort)"
trap cleanup EXIT

step "Building the deterministic Go test plugins (SDK examples) as local images"
for t in llm embedder vector-store persistence queue policy; do
  docker build -q -t "llmcache-e2e/$t:1" "sdk/examples/$t" >/dev/null
  echo "  llmcache-e2e/$t:1"
done

step "Creating local git repositories for the Python plugins"
rm -rf "$E2E_REPOS"; mkdir -p "$E2E_REPOS"
for t in embedding-model vector-index similarity-metric; do
  cp -r "sdk/examples/$t" "$E2E_REPOS/$t"
  ( cd "$E2E_REPOS/$t" && git init -q -b main && git add -A \
    && git -c user.name=e2e -c user.email=e2e@example.invalid commit -q -m "example" )
  echo "  /e2e-repos/$t"
done
chmod -R a+rX "$E2E_REPOS"

step "Starting the normal stack (project $PROJECT) with the plugin platform"
"${COMPOSE[@]}" up --build -d

step "Waiting for the stack to become healthy"
deadline=$((SECONDS + 400))
until curl -fs "http://127.0.0.1:$E2E_PORT/health" 2>/dev/null | grep -q '"status" *: *"ok"'; do
  if [ "$SECONDS" -ge "$deadline" ]; then
    "${COMPOSE[@]}" ps; "${COMPOSE[@]}" logs --tail=40 orchestrator plugin-controller; echo "stack not healthy" >&2; exit 1
  fi
  sleep 3
done
echo "healthy"

if [ "${E2E_SKIP_TEST:-0}" = "1" ]; then
  echo "E2E_SKIP_TEST=1: stack is up; skipping the acceptance test."; exit 0
fi

step "Running the acceptance test"
E2E_STACK_URL="http://127.0.0.1:$E2E_PORT" \
E2E_COMPOSE="${COMPOSE[*]}" \
  go test -count=1 -timeout 45m -v ./test/e2e -run TestPluginPlatformAcceptance
