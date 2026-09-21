#!/usr/bin/env bash
# Build llm-cache-plugin from this checkout and verify a plugin directory with it.
#   sdk/contract-tests/run.sh sdk/examples/queue
#   sdk/contract-tests/run.sh path/to/your/plugin --endpoint http://127.0.0.1:8080
set -euo pipefail
root="$(cd "$(dirname "$0")/../.." && pwd)"
dir="${1:?usage: run.sh PLUGIN_DIR [llm-cache-plugin verify flags]}"
shift || true
bin="$(mktemp -d)/llm-cache-plugin"
( cd "$root" && go build -o "$bin" ./cmd/llm-cache-plugin )
LLM_CACHE_RUNNER_DIR="$root" exec "$bin" verify "$dir" "$@"
