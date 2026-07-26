# LLM Semantic Cache — Build Plan

A transparent, look-aside **semantic cache** in front of a remote LLM. On each
prompt the system embeds the text, runs a vector similarity search, and returns
a cached reply when something is "close enough" (cache **hit**); otherwise it
calls the LLM and stores the new pair (cache **miss**).

This plan tracks how we go from the scaffold in this repo to the full system
described in the HLD.

## Strategy

We follow the HLD stack exactly (Go orchestrator + Python ML services + Redis +
Docker) and build a **walking skeleton first**: all services wired together
end-to-end with *stub* internals, then we replace each stub with the real
implementation. The stubs already speak the final REST contracts, so each
upgrade is local to one service.

| Concern | Current state | Remaining target |
|---|---|---|
| Embeddings | sentence-transformers `all-MiniLM-L6-v2` | Threshold/model tuning |
| Vector search | FAISS index with id map, rebuilt from Redis on startup | Optional persisted FAISS index |
| LLM | stub mode + OpenAI Responses API backend with timeout/retry | Provider-specific tuning |
| Persistence | Redis-backed policy-agnostic cache entries | Optional retry/dead-letter handling |
| Async update | Redis Streams queue + worker | Optional pending-job recovery improvements |
| Eviction | Pluggable LRU/LFU via Policy Manager | Optional LFU aging/optimization |

## Architecture (target)

```
CLI ──HTTP──> Orchestrator (Go) ──> Embedding Service (Py/FastAPI)
                   │             ──> Vector Store Service (Py/FastAPI, FAISS)
                   │             ──> LLM Backend (OpenAI)
                   └────────────────> Redis (persistence + async queue)
```

The orchestrator depends only on interfaces (`EmbedderClient`,
`VectorStoreClient`, `LLMBackend`, `PersistenceStore`, `PolicyManager`), so the
embedding model, vector DB, and LLM provider are all replaceable.

## REST contracts (current)

**Orchestrator** (`:8080`)
- `POST /query` `{prompt}` → `{reply, cache_hit, distance, latency_ms, source}`
- `GET /stats` → `{hits, misses, hit_rate, size, policy}`
- `POST /flush` → clears cache + counters
- `POST /policy` `{policy}` → validate current startup policy; changing policy
  requires `CACHE_POLICY` + restart
- `GET /health`

**Embedding service** (`:8001`)
- `POST /embed` `{text}` → `{vector, dim}`
- `GET /model-info`, `GET /health`

**Vector store service** (`:8002`)
- `POST /search` `{vector, top_k, threshold}` → `{hit, id, distance}`
- `POST /upsert` `{vector}` → `{id}`
- `POST /rebuild` `{entries:[{id, vector}]}` → `{restored}`
- `DELETE /entries/{id}`, `GET /size`, `POST /flush`, `GET /health`

> Distance convention: **cosine distance** (`1 - cosine similarity`). Lower is
> more similar; a hit requires `distance <= threshold`.

## Milestones (maps to HLD §1.6 week table)

- [x] **Wk 1–4 — Scaffold & contracts.** Repo structure, REST contracts,
  walking skeleton wired end-to-end with stubs, docker-compose, this plan.
- [x] **Wk 5 — Real embeddings.** Replace `embed_text` with sentence-transformers
  `all-MiniLM-L6-v2`. Keep `dim=384` so it's a drop-in.
- [x] **Wk 6 — FAISS.** Swap the in-memory store for a FAISS index + id map.
- [x] **Wk 7 — Orchestrator hardening.** Health fan-out to embedding,
  vectorstore, and Redis; downstream clients use timeouts.
- [x] **Wk 8 — LLM + Redis.** OpenAI backend with timeout/retry, RedisStore,
  and rebuild-from-Redis are implemented.
- [x] **Wk 9 — Policies + async queue.** LRU/LFU eviction and Redis Streams
  cache-update worker are implemented.
- [ ] **Wk 10 — CLI polish.** `query`, `stats`, `flush`, `policy` (skeleton done);
  add nicer output and error messages.
- [ ] **Wk 11 — Benchmark & test.** Threshold tuning on a representative prompt
  set; integration tests; measure hit rate and latency.
- [ ] **Wk 12 — Deliver.** Finalize deploy, demo, docs, submit.

## Open issues (from HLD §1.5 — decide as we go)

1. Default similarity **threshold** — tune empirically once real embeddings land.
2. **Long prompts** — reject / truncate / chunk / summarize before embedding?
3. **FAISS persistence** — current decision: rebuild from Redis on startup.
4. **LLM retry** behavior on timeout / provider error — current version retries
   failed OpenAI requests up to 3 attempts.
5. **Metrics** stack — candidate Prometheus + Grafana (secondary feature).
6. **Web chat UI** — final submission or follow-up extension?

## Session log

- **2026-07-26 — Pluggable-seam refactor merged & verified.** Introduced a
  generic `internal/plugin.Registry[T]` in Go and an equivalent
  `app/plugins/` auto-discovery mechanism in each Python service, so every
  seam (embedder, vector store, LLM, persistence, queue, eviction policy on
  the Go side; embedding model, vector index, similarity metric on the
  Python side) self-registers via `init()` / `@register(...)` — adding a new
  backend now means dropping in one file, no existing file changes. Verified
  end-to-end after merging: `go build`/`vet`/`test`/`test -race` all clean,
  all four Docker images build, and a full `docker-compose` run confirmed the
  real miss → LLM → async cache-write → hit cycle, `/stats`, `/health`,
  `/policy`, `/flush`, and FAISS-index rebuild-from-Redis after an
  orchestrator restart all behave as documented. No bugs found.

## Division of work (3-person team)

Roughly along service boundaries, which is the point of the architecture:
- **Embedding + Vector Store** (Python/FastAPI, ML) — one owner.
- **Orchestrator + LLM client** (Go) — one owner.
- **Persistence/Redis + policies + CLI + deploy** — one owner.

Integration points are the REST contracts above; agree on contract changes
before implementing.
