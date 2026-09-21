#!/usr/bin/env bash
# One-command demo of the plugin platform: enables it, starts the stack, and installs
# and activates the example LLM plugin so you land on the same Plugins page everyone sees.
#
#   scripts/demo_plugins.sh              # set up, start, install + activate the demo plugin
#   scripts/demo_plugins.sh --empty      # set up and start only; add the plugin yourself in the UI
#
# It uses whatever LLM_MODE your .env sets (the default in .env.example is the stub: no API key needed).
It never overwrites an existing .env: it only appends the plugin
# settings that are missing. Needs docker (with compose), curl and python3.
set -euo pipefail
cd "$(dirname "$0")/.."

EMPTY=0; [ "${1:-}" = "--empty" ] && EMPTY=1
step() { printf '\n==> %s\n' "$*"; }
val()  { grep -E "^$1=" .env | tail -1 | cut -d= -f2- | tr -d '\r'; }
set_default() { # set KEY=VALUE only when KEY has no non-empty value yet (an empty KEY= line is replaced)
  [ -n "$(val "$1" || true)" ] && return 0
  sed -i "/^$1=\r\?\$/d" .env
  printf '%s=%s\n' "$1" "$2" >> .env
}

command -v docker >/dev/null || { echo "docker is required" >&2; exit 1; }
docker compose version >/dev/null 2>&1 || { echo "docker compose is required" >&2; exit 1; }

step "Preparing .env"
[ -f .env ] || { cp .env.example .env; echo "created .env from .env.example"; }
grep -q '^# --- Plugin platform (demo)' .env || printf '\n# --- Plugin platform (demo) ---\n' >> .env
if [ "$(val ENABLE_PLUGIN_INSTALLATION || true)" != "true" ]; then
  sed -i '/^ENABLE_PLUGIN_INSTALLATION=/d' .env
  echo "ENABLE_PLUGIN_INSTALLATION=true" >> .env
fi
set_default ADMIN_TOKEN "$(openssl rand -hex 24)"
set_default PLUGIN_SECRET_KEY "$(openssl rand -base64 32)"
set_default PLUGIN_CONTROLLER_URL "http://plugin-controller:8090"
set_default PLUGIN_CONTROLLER_TOKEN "$(openssl rand -hex 24)"
set_default PLUGIN_ALLOW_LOCAL_IMAGES "true"
# A value present but empty (as in .env.example) is replaced rather than duplicated.
for k in ADMIN_TOKEN PLUGIN_SECRET_KEY PLUGIN_CONTROLLER_TOKEN; do
  [ -n "$(val $k || true)" ] || { echo "failed to set $k" >&2; exit 1; }
done
TOKEN="$(val ADMIN_TOKEN | tail -1)"

step "Building the example plugin image (sdk/examples/llm)"
docker build -q -t my-llm:1 sdk/examples/llm >/dev/null
echo "my-llm:1 built"

step "Starting the stack (the first build is slow: it includes the embedding image)"
docker compose --profile plugins up --build -d

step "Waiting for the stack to become healthy"
deadline=$((SECONDS + 420))
until curl -fs http://localhost:8080/health 2>/dev/null | grep -q '"status" *: *"ok"'; do
  [ "$SECONDS" -lt "$deadline" ] || { docker compose --profile plugins ps; echo "not healthy in time (is port 8080 free?)" >&2; exit 1; }
  sleep 4
done
echo healthy

api() { # api METHOD PATH [JSON]
  curl -fsS -X "$1" "http://localhost:8080$2" -H "Authorization: Bearer $TOKEN" \
       -H 'Content-Type: application/json' ${3:+--data "$3"}
}

if [ "$EMPTY" = 0 ]; then
  step "Installing and activating the demo LLM plugin"
  existing=$(api GET /admin/plugins | python3 -c 'import sys,json; print(next((p["id"] for p in json.load(sys.stdin)["plugins"] if p["name"]=="sdk-echo-llm" and p["state"] in ("verified","active","inactive")),""))')
  if [ -z "$existing" ]; then
    body=$(python3 - <<'PY'
import json
print(json.dumps({"type":"llm","mode":"image","image":"my-llm:1",
  "manifest":open("sdk/examples/llm/plugin.yaml").read(),"config":{"model":"demo"}}))
PY
)
    existing=$(api POST /admin/plugins/install "$body" | python3 -c 'import sys,json; print(json.load(sys.stdin)["plugin"]["id"])')
    echo "installing $existing ..."
  fi
  for _ in $(seq 1 90); do
    state=$(api GET "/admin/plugins/$existing" | python3 -c 'import sys,json; print(json.load(sys.stdin)["plugin"]["state"])')
    case "$state" in verified|active|inactive) break ;; failed) echo "plugin failed; see the UI logs" >&2; exit 1 ;; esac
    sleep 2
  done
  [ "$state" = active ] || api POST "/admin/plugins/$existing/activate" '{}' >/dev/null
  echo "demo plugin active"
  q() { curl -fsS -X POST http://localhost:8080/query -H 'Content-Type: application/json' -d "{\"prompt\":\"$1\"}"; }
  echo; echo "First ask (cache miss, answered by the plugin):"; q "Explain how volcanoes erupt" | python3 -m json.tool | grep -E '"(reply|cache_hit|source)"'
  sleep 1
  echo "Same question again (cache hit):";                  q "Explain how volcanoes erupt" | python3 -m json.tool | grep -E '"(reply|cache_hit|source)"'
fi

cat <<MSG

==> Ready
  Plugins page : http://localhost:8080/plugins
  Query page   : http://localhost:8080/
  Admin token  : $TOKEN      (also in .env as ADMIN_TOKEN; keep it private)
  Stop it      : docker compose --profile plugins down
MSG
