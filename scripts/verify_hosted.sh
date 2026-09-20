#!/usr/bin/env bash
# End-to-end check of a running deployment (local or hosted): confirms that a
# visitor really gets a real answer, that repeats are served from cache, and
# that admin endpoints are locked.
#
# Usage: scripts/verify_hosted.sh <base-url> [--expect-locked]
#   e.g. scripts/verify_hosted.sh https://cache.example.com --expect-locked
#
# --expect-locked  also require POST /flush without a token to be rejected
#                  (use for hosted deployments; ADMIN_TOKEN must be set).
# The check asks a unique question, so it adds one entry to the cache and never
# flushes anything.
set -uo pipefail

BASE="${1:-}"
[ -n "$BASE" ] || { sed -n '2,10p' "$0"; exit 2; }
BASE="${BASE%/}"
EXPECT_LOCKED=0
[ "${2:-}" = "--expect-locked" ] && EXPECT_LOCKED=1

PASS=0
FAILED=0
ok()   { PASS=$((PASS + 1)); printf 'PASS  %s\n' "$*"; }
bad()  { FAILED=$((FAILED + 1)); printf 'FAIL  %s\n' "$*"; }

command -v curl >/dev/null 2>&1 || { echo "curl is required" >&2; exit 2; }

# 1. Health
health="$(curl -fsS -m 15 "$BASE/health" 2>&1)"
if echo "$health" | grep -q '"status" *: *"ok"'; then ok "health is ok: $health"; else bad "health: $health"; fi

# 2. Web UI
ui="$(curl -fsS -m 15 "$BASE/" 2>&1)"
if echo "$ui" | grep -qi '<html'; then ok "web UI is served at /"; else bad "web UI not served at /"; fi

# 3. A real question gets a real answer (a unique prompt forces a cache miss)
prompt="In one sentence, what is a semantic cache? (verify $(date +%s)-$RANDOM)"
body="{\"prompt\":\"$prompt\"}"
first="$(curl -sS -m 120 -H 'Content-Type: application/json' -d "$body" "$BASE/query" 2>&1)"
echo "first reply : $first"
echo "$first" | grep -Eq '"reply" *: *"[^"]{2,}' && ok "answer is non-empty" || bad "answer is empty or the request failed"
echo "$first" | grep -q 'stub-llm' && bad "answer is the stub placeholder, not a real LLM (check LLM_MODE / GEMINI_API_KEY)" || ok "answer is not a stub placeholder"
echo "$first" | grep -q '"cache_hit" *: *false' && ok "first ask was a cache miss served by the LLM" || bad "first ask was not a fresh LLM answer"

# 4. Asking again is served from cache (cache updates are applied asynchronously)
sleep 3
second="$(curl -sS -m 60 -H 'Content-Type: application/json' -d "$body" "$BASE/query" 2>&1)"
echo "second reply: $second"
echo "$second" | grep -q '"cache_hit" *: *true' && ok "repeat ask was a cache hit" || bad "repeat ask was not a cache hit"

# 5. Admin endpoints are locked without a token
if [ "$EXPECT_LOCKED" -eq 1 ]; then
  code="$(curl -s -o /dev/null -w '%{http_code}' -m 15 -X POST "$BASE/flush")"
  [ "$code" = "401" ] && ok "POST /flush without a token is rejected (401)" || bad "POST /flush without a token returned $code, expected 401"
  code="$(curl -s -o /dev/null -w '%{http_code}' -m 15 -X POST -H 'Content-Type: application/json' -d '{"policy":"lru"}' "$BASE/policy")"
  [ "$code" = "401" ] && ok "POST /policy without a token is rejected (401)" || bad "POST /policy without a token returned $code, expected 401"
fi

printf '\n%s passed, %s failed\n' "$PASS" "$FAILED"
[ "$FAILED" -eq 0 ]
