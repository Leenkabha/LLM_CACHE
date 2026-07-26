# Demo script

Everything below was tested live against the real Gemini backend before this
was written. Actual numbers observed: cache **miss** ~23s (real LLM call),
cache **hit** ~100-300ms (75-200x faster).

## Before you present

1. `cp .env.example .env` if you don't already have one, then set:
   ```
   LLM_MODE=gemini
   GEMINI_API_KEY=<your key>
   GEMINI_MODEL=gemini-flash-latest
   ```
   `gemini-2.0-flash` returns HTTP 429 (quota limit 0) on the free tier for
   this key — `gemini-flash-latest` is the one confirmed working.

2. Start everything **10-15 minutes before** you present, not right before:
   ```bash
   docker compose up --build -d
   ```
   The embedding service loads a real sentence-transformers model on boot,
   which takes ~10-30s after the container reports "started". Confirm
   everything is actually ready:
   ```bash
   curl -s localhost:8080/health
   ```
   Wait for `"status":"ok"` with all three checks `"ok"` before doing anything
   else.

3. Do one throwaway query to confirm the Gemini key still works right now
   (quota/keys can expire or hit limits between when you tested and when you
   present):
   ```bash
   curl -s localhost:8080/query -H "Content-Type: application/json" \
     -d '{"prompt":"warmup"}'
   ```
   If this errors, see Troubleshooting below — better to find out now than
   live.

4. Flush the cache so the demo starts from a clean state:
   ```bash
   curl -s -X POST localhost:8080/flush
   ```

## Live script

Say what you're doing at each step — don't just run commands silently.

**1. Show it's a real multi-service system, not a script**
```bash
curl -s localhost:8080/health
```
Point out: orchestrator (Go) checking embedding service, vector store, and
Redis are all up — this is 4 containers talking over HTTP, not one program.

**2. First question — cache miss, real LLM call**
```bash
curl -s localhost:8080/query -H "Content-Type: application/json" \
  -d '{"prompt":"What is virtual memory?"}'
```
This takes ~15-25 seconds. Narrate while it runs: "No cached answer exists
yet, so this is going out to a real Gemini model over the network right now."
Point out `"cache_hit":false`, `"source":"llm"`, and the `latency_ms` once it
lands.

**3. Same question again — cache hit**
```bash
curl -s localhost:8080/query -H "Content-Type: application/json" \
  -d '{"prompt":"What is virtual memory?"}'
```
Near-instant. Point out `"cache_hit":true`, `"distance":0`, and the
`latency_ms` — compare directly against step 2's number out loud.

**4. The actual point of a *semantic* cache — a paraphrase**
```bash
curl -s localhost:8080/query -H "Content-Type: application/json" \
  -d '{"prompt":"Can you explain what virtual memory is?"}'
```
Different wording, never seen before, still a cache hit (`distance` around
0.03-0.04, well under the 0.25 threshold). This is the headline result: it's
not exact-string caching, it's matching by *meaning* via sentence embeddings.

**5. An unrelated question — correctly falls through to the LLM**
```bash
curl -s localhost:8080/query -H "Content-Type: application/json" \
  -d '{"prompt":"How do I bake sourdough bread?"}'
```
Shows the threshold isn't just saying yes to everything.

**6. Stats**
```bash
curl -s localhost:8080/stats
```
`hits`, `misses`, `hit_rate`, cache `size`, active `policy`.

**7. (Optional, if asked about resilience) Restart survives**
```bash
docker compose restart orchestrator
curl -s localhost:8080/query -H "Content-Type: application/json" \
  -d '{"prompt":"What is virtual memory?"}'
```
Still a hit after restart — the FAISS index was rebuilt from Redis on boot,
not lost.

## Troubleshooting

- **`embedding service unreachable` on first curl after `up`** — the model
  hasn't finished loading yet, wait longer (see step 2 above).
- **Gemini 429 `quota exceeded`, `limit: 0`** — wrong model name for this
  key's tier; `gemini-flash-latest` is confirmed working, `gemini-2.0-flash`
  and `gemini-2.5-flash` are not on this key.
- **Gemini 429 with a nonzero limit / "retry in Ns"** — you're actually
  rate-limited (free tier is a handful of requests/minute). Wait it out, or
  fall back to `LLM_MODE=stub` in `.env` + `docker compose up -d orchestrator`
  as a live fallback (loses the network-latency wow-factor but keeps the
  semantic-matching wow-factor).
- **Nothing responds at all** — check `docker compose ps`, then
  `docker compose logs orchestrator` for the actual error.

## Recorded backup

Run this exact script once end-to-end while screen-recording (Win+Alt+R on
Windows, or OBS) as insurance against live Docker/network failure in the
room. Re-run step 0 (`flush`) before recording so the miss/hit numbers are
real, not stale from earlier testing.
