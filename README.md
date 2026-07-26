# LLM Semantic Cache

A **semantic cache** that sits in front of a remote LLM. When someone asks a question similar to one already answered, the system returns the cached answer instantly instead of calling the (slow, expensive) LLM again.

---

## How it works

```
Your prompt
     │
     ▼
Orchestrator :8080 (Go)
     │
     ├── 1. Embedder :8001 (Python)      → turns your text into a vector (numbers)
     ├── 2. Vector Store :8002 (Python)  → finds a similar vector already cached
     │
     ├── HIT  → returns cached reply instantly
     └── MISS → calls LLM, caches the result, returns the reply
```

| Service | Language | Port | What it does |
|---------|----------|------|-------------|
| Orchestrator | Go | 8080 | Brain — drives the full flow |
| Embedding service | Python | 8001 | Converts text → vector |
| Vector store service | Python | 8002 | Stores & searches vectors |
| Redis | — | 6379 | Persistent cache-entry storage, async queue, and restart recovery |

---

## Prerequisites — install these first

### 1. Git
```bash
# Ubuntu / Debian
sudo apt update && sudo apt install -y git

# macOS
brew install git

# Windows
# Download from https://git-scm.com/download/win
```

### 2. Docker Desktop
Docker runs all services in containers so you don't need to install Go or Python manually.

- **Windows / macOS:** Download from https://www.docker.com/products/docker-desktop  
  After install, open Docker Desktop and wait for it to say "Engine running".
- **Ubuntu / Debian:**
```bash
sudo apt update
sudo apt install -y ca-certificates curl gnupg
sudo install -m 0755 -d /etc/apt/keyrings
curl -fsSL https://download.docker.com/linux/ubuntu/gpg | sudo gpg --dearmor -o /etc/apt/keyrings/docker.gpg
echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.gpg] https://download.docker.com/linux/ubuntu $(. /etc/os-release && echo $VERSION_CODENAME) stable" | sudo tee /etc/apt/sources.list.d/docker.list
sudo apt update
sudo apt install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
sudo usermod -aG docker $USER   # lets you run docker without sudo
newgrp docker                   # apply group change in current terminal
```

Verify Docker works:
```bash
docker --version
docker compose version
```

### 3. Node.js (required for Claude Code)
- **Windows / macOS:** Download from https://nodejs.org (LTS version)
- **Ubuntu / Debian:**
```bash
curl -fsSL https://deb.nodesource.com/setup_lts.x | sudo -E bash -
sudo apt install -y nodejs
```

Verify:
```bash
node --version   # should print v18 or higher
```

### 4. Claude Code (AI assistant in your terminal)
```bash
npm install -g @anthropic-ai/claude-code
```

Then launch it:
```bash
claude
```

On first launch it will ask you to log in with your Anthropic account. Follow the link it prints.

After login, run it from inside the project folder:
```bash
cd LLM_CACHE
claude
```

You can now ask questions about the code directly in your terminal, e.g.:
- `what does the orchestrator do?`
- `show me where the cache hit logic is`
- `explain the /query endpoint`

---

## Getting the project

```bash
git clone https://github.com/Leenkabha/LLM_CACHE.git
cd LLM_CACHE
```

---

## Running the project

### Step 1 — create your config file
```bash
cp .env.example .env
```
The defaults work out of the box. No changes needed to get started.

### Step 2 — build and start all services
```bash
docker compose up --build
```

This starts 4 containers: orchestrator, embedding service, vector store, and Redis.  
Wait until you see lines like:
```
llm_cache-embedding-1     | INFO:     Application startup complete.
llm_cache-vectorstore-1   | INFO:     Application startup complete.
llm_cache-orchestrator-1  | orchestrator listening on :8080
```

### Step 3 — verify everything is up (new terminal)
```bash
curl -s localhost:8080/health
curl -s localhost:8001/health
curl -s localhost:8002/health
```

The embedding and vector-store services return: `{"status":"ok"}`.

The orchestrator returns a dependency-aware health response:
```json
{
  "status": "ok",
  "checks": {
    "embedding": "ok",
    "vectorstore": "ok",
    "redis": "ok"
  }
}
```

---

## Using the cache

### Send a prompt (cache miss — first time)
```bash
curl -s localhost:8080/query \
  -H "Content-Type: application/json" \
  -d '{"prompt":"What is virtual memory?"}'
```

Response:
```json
{
  "reply": "[stub-llm] This is a placeholder reply for: \"What is virtual memory?\"",
  "cache_hit": false,
  "distance": -1,
  "latency_ms": 38,
  "source": "llm"
}
```

### Send the exact same prompt again (cache hit)
```bash
curl -s localhost:8080/query \
  -H "Content-Type: application/json" \
  -d '{"prompt":"What is virtual memory?"}'
```

Response:
```json
{
  "reply": "[stub-llm] This is a placeholder reply for: \"What is virtual memory?\"",
  "cache_hit": true,
  "distance": 0.0,
  "latency_ms": 4,
  "source": "cache"
}
```

Notice: `cache_hit: true`, `source: "cache"`, and much lower `latency_ms`.

### Check hit/miss statistics
```bash
curl -s localhost:8080/stats
```

### Wipe the cache
```bash
curl -s -X POST localhost:8080/flush
```

### Eviction policy

The active eviction policy is selected at startup with `CACHE_POLICY=lru` or
`CACHE_POLICY=lfu`.

Runtime policy switching is intentionally not supported because LRU and LFU own
different policy-specific metadata. Calling `/policy` with the current policy is
accepted; calling it with a different policy returns an error telling you to
change `CACHE_POLICY` and restart.

```bash
curl -s -X POST localhost:8080/policy \
  -H "Content-Type: application/json" \
  -d '{"policy":"lru"}'
```

---

## Understanding the response fields

| Field | Meaning |
|-------|---------|
| `reply` | The answer text |
| `cache_hit` | `true` = served from cache, `false` = called the LLM |
| `distance` | How similar the prompt was to the cached one (0.0 = identical, -1 = no match found) |
| `latency_ms` | Total time to respond in milliseconds |
| `source` | `"cache"` or `"llm"` |

---

## Current status — what is real vs stubbed

| Component | Current state | Planned replacement |
|-----------|--------------|-------------------|
| Embedder | Real sentence-transformers model: `all-MiniLM-L6-v2` | Tune model/threshold if needed |
| Vector Store | FAISS `IndexIDMap` in RAM, rebuilt from Redis on startup | Optional persisted FAISS index |
| LLM | `stub` mode for local demos; `openai` mode via OpenAI Responses API | Provider-specific tuning |
| Persistence | Redis-backed cache entries: `id`, `prompt`, `reply`, `vector`, `created_at` | Optional retry/dead-letter handling |
| Async update | Redis Streams queue + worker | Optional pending-job recovery improvements |
| Eviction | Pluggable LRU/LFU policy metadata, selected at startup | Optional LFU aging/optimized LFU |

Cache entries are intentionally policy-agnostic. LRU owns recency metadata and
LFU owns frequency metadata separately, so the cache schema does not depend on a
specific policy.

---

## Pluggability — the six replaceable seams

Every part of the system that the spec calls "interchangeable" is a real
interface (a *port*) with implementations (*adapters*) that **register
themselves**. **Adding a new backend means dropping in one new file that
registers itself — you edit no existing file, not the orchestrator, not a
factory.** The Go orchestrator depends only on interfaces, so it is also
unit-testable with in-memory doubles via `orchestrator.NewWithDependencies`.

| Seam | Interface (port) | Adapters today | Selector |
|------|------------------|----------------|----------|
| Embedder (orchestrator → service) | `embedder.Embedder` | `http` | `EMBEDDING_BACKEND` |
| Vector store (orchestrator → service) | `vectorstore.VectorStore` | `http` | `VECTORSTORE_BACKEND` |
| LLM backend | `llm.Backend` | `stub`, `openai`, `gemini` | `LLM_MODE` |
| Persistence | `persistence.Store` | `redis`, `memory` | `PERSISTENCE_BACKEND` |
| Async queue | `cachequeue.Queue` | `redis` | `QUEUE_BACKEND` |
| Eviction policy | `policy.EvictionPolicy` | `lru`, `lfu` | `CACHE_POLICY` |
| Embedding model *(inside embedding service)* | `EmbeddingModel` | `sentence-transformers` | `EMBEDDING_MODEL_BACKEND` |
| Vector index *(inside vector-store service)* | `VectorIndex` | `faiss` | `VECTOR_INDEX_BACKEND` |
| Similarity metric *(inside vector-store service)* | `SimilarityMetric` | `cosine`, `euclidean` | `SIMILARITY_METRIC` |

> Note: `EMBEDDING_BACKEND`/`VECTORSTORE_BACKEND` select the **orchestrator's
> client adapter**; the `*_MODEL_BACKEND` / `VECTOR_INDEX_BACKEND` variables
> select the engine **inside** each Python service. They are deliberately
> distinct names so the two containers never clash.

**How self-registration works.** Each seam has a registry. Built-in adapters
call `Register(...)` (Go, from an `init()`) or `@register(...)` (Python) so they
add themselves at startup. To plug in your own:

- **Go:** add a file to the seam's package (e.g. `internal/llm/`) with a type
  implementing the interface and an `init()` that calls `llm.Register("name", …)`.
  Because it's in the same package it's compiled and auto-registers — no imports,
  no edits elsewhere. Then set the selector env var.
- **Python:** drop a file into the service's `app/plugins/` directory that
  subclasses the interface and calls `@register("name")`. It's auto-imported at
  startup. Then set the selector env var.

Unknown selector names **fail fast at startup** listing what's registered.

---

## Team split

| Member | Task | Files to work in |
|--------|------|-----------------|
| A | Real embedder (ML model) | `embedding_service/app/models.py`, `internal/embedder/client.go` |
| B | Real vector store (FAISS) | `vector_store_service/app/index.py`, `vector_store_service/app/metrics.py`, `internal/vectorstore/client.go` |
| C | Real LLM + Redis persistence/policies | `internal/llm/client.go`, `internal/persistence/`, `internal/policy/` |

The main service flow lives in `internal/orchestrator/service.go`, which wires
the adapters together in `BuildDependencies`.

---

## Stopping the project

```bash
# stop containers but keep their state
docker compose stop

# stop and remove containers completely
docker compose down
```

---

## Project structure

```
LLM_CACHE/
├── cmd/
│   ├── orchestrator/main.go      # orchestrator entry point
│   └── cli/main.go               # command-line client
├── internal/
│   ├── orchestrator/service.go   # request flow + BuildDependencies wiring
│   ├── config/config.go          # env var configuration (+ backend selectors)
│   ├── cachequeue/redis_stream.go # Queue interface + Redis Streams adapter + factory
│   ├── embedder/client.go        # Embedder interface + HTTP adapter + factory
│   ├── vectorstore/client.go     # VectorStore interface + HTTP adapter + factory
│   ├── llm/client.go             # Backend interface + stub/openai/gemini + factory
│   ├── persistence/store.go      # Store interface + memory adapter + factory
│   ├── persistence/redis_store.go # Redis-backed Store adapter
│   ├── policy/policy.go          # EvictionPolicy interface + Manager + factory
│   ├── policy/lru.go             # LRU adapter
│   └── policy/lfu.go             # LFU adapter
├── embedding_service/
│   └── app/
│       ├── main.py               # thin FastAPI layer
│       └── models.py             # EmbeddingModel interface + adapter + factory
├── vector_store_service/
│   └── app/
│       ├── main.py               # thin FastAPI layer
│       ├── index.py              # VectorIndex interface + FAISS adapter + factory
│       └── metrics.py            # SimilarityMetric interface + cosine/euclidean
├── deploy/                       # Dockerfiles
├── docker-compose.yml            # runs all services together
├── .env.example                  # config template (all pluggable selectors)
└── spec1.pdf / spec 2.pdf        # functional spec & HLD
```
