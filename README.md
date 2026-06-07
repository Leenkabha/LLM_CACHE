# LLM Semantic Cache

A transparent **semantic cache** in front of a remote LLM. Repeated or
near-duplicate prompts are served from cache instead of re-calling the model,
cutting latency, cost, and API usage.

> **Status: v0 walking skeleton.** All services are wired together end-to-end
> with stub internals (hash embeddings, brute-force search, canned LLM replies,
> in-memory persistence). See [`PLAN.md`](./PLAN.md) for the roadmap to the full
> system.

## Architecture

```
CLI ──HTTP──> Orchestrator (Go) ──> Embedding Service (Python/FastAPI)
                   │             ──> Vector Store Service (Python/FastAPI)
                   │             ──> LLM Backend (stub → OpenAI)
                   └────────────────> Redis (reserved; persistence in wk 8)
```

| Component | Tech | Port | Dir |
|---|---|---|---|
| Orchestrator | Go | 8080 | `cmd/orchestrator`, `internal/` |
| CLI client | Go | — | `cmd/cli` |
| Embedding service | Python/FastAPI | 8001 | `embedding_service/` |
| Vector store service | Python/FastAPI | 8002 | `vector_store_service/` |
| Redis | redis:7 | 6379 | (compose only) |

## Run it (Docker)

```bash
cp .env.example .env          # defaults work out of the box (LLM_MODE=stub)
docker compose up --build     # starts embedding, vectorstore, redis, orchestrator
```

Then, in another terminal, use the CLI via compose:

```bash
docker compose run --rm cli query "What is virtual memory?"
docker compose run --rm cli query "Explain virtual memory"   # should hit cache
docker compose run --rm cli stats
docker compose run --rm cli flush
docker compose run --rm cli policy lfu
```

Or hit the orchestrator directly:

```bash
curl -s localhost:8080/query -d '{"prompt":"hello"}' | jq
curl -s localhost:8080/stats | jq
```

## Run it (local, no Docker)

```bash
# terminal 1 — embedding service
cd embedding_service && pip install -r requirements.txt
uvicorn app.main:app --port 8001

# terminal 2 — vector store service
cd vector_store_service && pip install -r requirements.txt
uvicorn app.main:app --port 8002

# terminal 3 — orchestrator (uses localhost defaults)
go run ./cmd/orchestrator

# terminal 4 — CLI
go run ./cmd/cli query "What is virtual memory?"
```

## Configuration

All via environment variables (see [`.env.example`](./.env.example)):
`SIMILARITY_THRESHOLD`, `CACHE_CAPACITY`, `CACHE_POLICY`, `EMBEDDING_URL`,
`VECTORSTORE_URL`, `REDIS_ADDR`, `LLM_MODE` (`stub`|`openai`), `OPENAI_API_KEY`,
`OPENAI_MODEL`.

## Specs

- `spec1.pdf` — Functional Specification
- `spec 2.pdf` — High-Level Design (HLD)
