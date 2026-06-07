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
| Redis | — | 6379 | Persistent reply storage (wired in week 8) |

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

Each should return: `{"status":"ok"}`

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

### Change eviction policy
```bash
curl -s -X POST localhost:8080/policy \
  -H "Content-Type: application/json" \
  -d '{"policy":"lfu"}'
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
| Embedder | Hash-based stub — not semantic | Real ML model: `all-MiniLM-L6-v2` |
| Vector Store | Brute-force in-memory search | FAISS index |
| LLM | Returns a hardcoded string | OpenAI API |
| Persistence | In-process memory | Redis |

Because the embedder is a stub, only **lexically similar** prompts (sharing the same words) will hit the cache right now. "What is virtual memory?" and "Explain virtual memory" will **not** hit each other until the real ML embedder is in place.

---

## Team split

| Member | Task | Files to work in |
|--------|------|-----------------|
| A | Real embedder (ML model) | `embedding_service/app/main.py`, `internal/embedder/client.go` |
| B | Real vector store (FAISS) | `vector_store_service/app/main.py`, `internal/vectorstore/client.go` |
| C | Real LLM + Redis persistence | `internal/llm/client.go`, `internal/persistence/store.go` |

All three are independent — nobody needs to touch `internal/orchestrator/service.go`.

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
│   ├── orchestrator/service.go   # full request flow logic
│   ├── config/config.go          # env var configuration
│   ├── embedder/client.go        # HTTP client → embedding service
│   ├── vectorstore/client.go     # HTTP client → vector store service
│   ├── llm/client.go             # LLM backend (stub / openai)
│   ├── persistence/store.go      # reply storage (memory / redis)
│   └── policy/policy.go          # eviction policy (lru / lfu)
├── embedding_service/
│   └── app/main.py               # FastAPI embedding service
├── vector_store_service/
│   └── app/main.py               # FastAPI vector store service
├── deploy/                       # Dockerfiles
├── docker-compose.yml            # runs all services together
├── .env.example                  # config template
└── spec1.pdf / spec 2.pdf        # functional spec & HLD
```
