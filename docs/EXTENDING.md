# Extension guide: custom cache policies and LLM providers

This guide is for developers extending this repository and operators selecting
those implementations. No changes to the orchestrator query flow are needed.

## What is pluggable, and for whom?

| Extension | Implementer | Runtime selector | Contract location |
| --- | --- | --- | --- |
| Eviction policy | Developer choosing which cached entry to remove | `CACHE_POLICY` | `internal/policy/policy.go`: `EvictionPolicy` |
| LLM provider | Developer connecting a completion API | `LLM_MODE` | `internal/llm/client.go`: `Backend` |
| Embedding client | Developer connecting an embedding service | `EMBEDDING_BACKEND` | `internal/embedder/client.go`: `Embedder` |
| Vector-store client | Developer connecting a vector service | `VECTORSTORE_BACKEND` | `internal/vectorstore/client.go`: `VectorStore` |
| Cache persistence | Developer changing reply storage | `PERSISTENCE_BACKEND` | `internal/persistence/store.go`: `Store` |
| Update queue | Developer changing asynchronous cache writes | `QUEUE_BACKEND` | `internal/cachequeue/redis_stream.go`: `Queue` |

Python index, metric, and embedding-model extensions have separate registries
in `vector_store_service/app/index.py`, `vector_store_service/app/metrics.py`,
and `embedding_service/app/models.py`. They are not Go plugins.

The worked examples below cover policies and LLMs. Go adapters are compiled into
the application: adding a source file requires rebuilding and restarting.
Environment variables select an already compiled registration; they do not load
arbitrary Go files or shared libraries at runtime. These are repository extension
points, not a separately versioned public SDK: Go's `internal` packages cannot be
imported by an unrelated external module.

## Quick start for operators

The normal defaults remain `CACHE_POLICY=lru` and `LLM_MODE=stub`.
To run the included FIFO policy and local custom HTTP provider, set these values
in your repository-root `.env` (create it from `.env.example` if needed):

```dotenv
CACHE_POLICY=fifo
LLM_MODE=example-http
EXAMPLE_LLM_URL=http://example-llm:8090/complete
EXAMPLE_LLM_MODEL=demo
EXAMPLE_LLM_TOKEN=
EXAMPLE_LLM_TIMEOUT=30s
CACHE_TOP_K=3
```

```bash
docker compose --profile example-llm up --build -d
curl http://localhost:8080/health
curl -X POST http://localhost:8080/query -H "Content-Type: application/json" \
  -d '{"prompt":"Explain the custom provider example"}'
```

The first cache miss returns a deterministic `[example demo] ...` reply. This
server demonstrates the protocol; it does not run an AI model. An existing cache
hit will return its cached reply without calling the new provider. The health
endpoint checks embedding, vector store, and Redis; use an uncached query to
verify the LLM connection. Do not clear an existing cache just to try the example.

For local Go development, run `go run ./examples/llm-server` in one terminal.
Set `EXAMPLE_LLM_URL=http://localhost:8090/complete` for a locally running
orchestrator. Inside Docker, `localhost` refers to that container; the bundled
Compose example uses its service name `example-llm` instead.

To switch back, restore your previous `CACHE_POLICY`/`LLM_MODE` and run
`docker compose up --build -d`. Stop the optional demo service separately with
`docker compose --profile example-llm stop example-llm` when no longer needed.

## Developer workflow

1. Copy the relevant working adapter below into another file in the same package.
2. Rename its types, constants, constructor, and registration key to avoid collisions.
3. Implement the documented contract and add algorithm/protocol-specific tests.
4. Run the reusable contract tests and the project tests.
5. Select the new name in the environment, rebuild, and restart.

Registration uses a package `init` function and a factory:

```go
func init() {
    Register("my-name", func(cfg config.Config) (Backend, error) {
        return newMyBackend(/* validated settings */)
    })
}
```

For policies the factory returns `EvictionPolicy`, not `Backend`. Import
`github.com/leenkabha/llm_cache/internal/config` in the new file. Use a unique name:
the current registry replaces an existing registration with the same name.
Register during initialization, not concurrently while the server is running.
Each factory should return fresh mutable state and report configuration errors
before any request is handled.

Files must be in the compiled package (`internal/policy` or `internal/llm`), not
only an unimported examples directory. If you use a separate package, the executable
must import it for its `init` registration to run.

## Custom eviction policy

Copy [`internal/policy/fifo.go`](../internal/policy/fifo.go). It is a complete,
registered FIFO implementation with mutex-protected state and an interface check:

```go
var _ EvictionPolicy = (*fifoPolicy)(nil)
```

The interface is:

```go
type EvictionPolicy interface {
    Name() string
    OnHit(id string)
    OnInsert(id string)
    OnDelete(id string)
    Victim() (string, bool)
    Flush()
}
```

| Method | Caller and required behavior |
| --- | --- |
| `Name` | Return the stable registration name, used in stats and logs. |
| `OnInsert` | Called after vector and reply storage succeed, before capacity enforcement; also replayed during startup recovery. Track the ID once. |
| `OnHit` | Called for each persisted reply actually returned by `/query`, including each Top-K result. Missing replies do not receive a hit. Unknown IDs must not create phantom entries. |
| `Victim` | When capacity is exceeded, select a tracked ID with `true`; return `false` when empty. Selection must not remove policy state. The orchestrator owns storage deletion. |
| `OnDelete` | Called after successful eviction from both persistence and vector storage. Remove the ID; repeated or unknown deletions should be harmless. |
| `Flush` | Clear all tracked IDs; the instance must remain reusable and its name unchanged. |

The policy receives IDs only, not prompts, reply text, vector distances, or TTLs.
It owns eviction metadata only, and must not call Redis/vector deletion itself.
If your algorithm needs additional per-entry data, that requires a separate
contract change rather than guessing it from the ID.

Hooks can overlap across HTTP handlers and the asynchronous cache worker. Protect
mutable state with a mutex (as FIFO does). Do not assume a multi-call sequence
such as `Victim` followed by `OnDelete` is atomic. Repeated `OnInsert` must not create
duplicate tracked IDs; whether it updates priority is algorithm-specific. FIFO
ignores hits and duplicate insertions; LRU may update recency.

On restart, the orchestrator reads persisted entries, sorts them by `CreatedAt`,
and replays `OnInsert`. Access counts and recency are not persisted by the current
policy interface. FIFO reconstructs insertion order except that equal timestamps
have no defined relative order. LRU/LFU restart with reconstructed metadata rather
than their previous access history. Runtime policy switching is not supported;
`POST /policy` rejects a change to a different name. Change the environment and
restart instead.

The orchestrator constructs policies with `policy.New(cfg)`, which forwards the
complete configuration to your factory (for example `cfg.Capacity`). The
`NewManager(name)` convenience constructor supplies only the selector; use `New`
if the policy depends on other fields. Custom settings outside `config.Config`
may be read from the factory's environment. Forward those variables in Compose
as described below.

### Reusable policy tests

See [`internal/policy/fifo_test.go`](../internal/policy/fifo_test.go).
Inside a test file in package `policy`, use:

```go
func TestMyPolicyContract(t *testing.T) {
    contracttest.PolicyContract(t, func() contracttest.Policy {
        p, err := registry.Build("my-name", config.Config{})
        if err != nil { t.Fatal(err) }
        return p
    })
}
```

Import `testing`, `internal/config`, and `internal/contracttest` using this module's
full import prefix. The suite tests empty state, unknown IDs, repeated inserts
and deletes, victim membership, reuse after flush, and concurrent hooks. It does
not prescribe FIFO/LRU/LFU victim order: add tests for your algorithm. The included
suite runs against all three built-in policies.

## Custom LLM provider

Copy [`internal/llm/example_http.go`](../internal/llm/example_http.go). The interface is:

```go
type Backend interface {
    Complete(ctx context.Context, prompt string) (string, error)
}
```

`Complete` runs only on a cache miss (including when all returned IDs lack stored
replies). Return completed reply text on success. On failure return an empty reply
and an error; the orchestrator responds with HTTP 502 and does not enqueue a cache
write. Successful replies are returned and queued for caching by the orchestrator.
The backend must not write to the cache itself.

The current interface is a single prompt/single text response. Streaming, tool
calls, chat history, and usage metadata are not part of this contract.

Requirements for an HTTP implementation:

- Pass `ctx` to `http.NewRequestWithContext`; honor cancellation and deadlines,
  including a context canceled before the call. Wrap context errors with `%w` or
  return them directly so callers can use `errors.Is`.
- Set a finite client timeout as well. The incoming request may have no deadline.
- A backend instance serves concurrent requests. Keep per-request state local and
  safely share its HTTP client; do not mutate global configuration per request.
- Validate configuration in the factory; check status, JSON structure, body size,
  and reply content. Close response bodies.
- Do not log tokens, authorization headers, private prompts, or raw provider error
  bodies. Returned errors are included in the current `/query` error response.
- If adding retries, bound them and make waits context-cancelable. Consider that
  retrying a generation request can incur another charge. The example does not retry.

### Example HTTP protocol

Request to the configured full URL:

```http
POST /complete
Content-Type: application/json
Authorization: Bearer <token-if-configured>

{"model":"demo","prompt":"hello"}
```

Successful response:

```json
{"reply":"The generated answer"}
```

The adapter requires HTTP 200 and a nonblank `reply`, accepts up to 1 MiB of
response data, and rejects redirects. Adapt the JSON structs and parsing to your
provider's actual protocol. This is not an OpenAI/Gemini compatibility claim.

| Setting | Meaning |
| --- | --- |
| `LLM_MODE=example-http` | Select the registered example adapter. |
| `EXAMPLE_LLM_URL` | Full HTTP(S) completion endpoint, required when running outside the supplied Compose defaults. |
| `EXAMPLE_LLM_MODEL` | Model identifier, required; Compose defaults to `demo`. |
| `EXAMPLE_LLM_TOKEN` | Optional bearer token; empty for the local demo server. |
| `EXAMPLE_LLM_TIMEOUT` | Positive Go duration, default `30s`. |

Use HTTPS for remote services. The included local demo server does not authenticate
requests; its published Compose port is bound to loopback. It is a development
example, not a production model server.

### Reusable LLM tests

[`internal/llm/example_http_test.go`](../internal/llm/example_http_test.go) creates a
local `httptest.Server`, sets configuration through `t.Setenv`, then calls:

```go
contracttest.LLMContract(t, func() contracttest.LLM {
    backend, err := New(config.Config{LLMMode: ModeExampleHTTP})
    if err != nil { t.Fatal(err) }
    return backend
}, "hello", "answer")
```

Use your own registration name and deterministic expected reply. The suite checks
successful completion, pre-cancellation, expired deadlines, and shared-instance
concurrent requests. Run against a local fake provider, never a billed endpoint.
The HTTP adapter's own tests additionally cover request headers/body, invalid
configuration, provider errors, malformed/oversized/empty responses, redirects,
in-flight cancellation, and client timeout. These tests are a baseline, not a
proof of correctness or a performance benchmark.

## Configuration forwarding and deployment

`config.Load` reads the core selectors. Custom factories can read their own
variables with `os.Getenv`, without extending `config.Config`. Docker Compose does
not automatically forward every `.env` value into the container: add your setting
under `services.orchestrator.environment`, for example:

```yaml
MY_LLM_URL: "${MY_LLM_URL:-}"
MY_LLM_TOKEN: "${MY_LLM_TOKEN:-}"
```

Do not put real credentials into checked-in example files. The new example variables
are already forwarded by the supplied Compose file. The orchestrator Dockerfile
copies `internal/` and `cmd/`, so same-package adapter files are included on rebuild.
Unknown selector names fail at startup with the registered choices listed.
Changing a selector does not clear existing cached responses.

## Validation commands

```bash
go test ./...
go vet ./...
go test -race ./internal/policy ./internal/llm
```

The race detector requires a supported Go/CGO toolchain and C compiler. If your
host lacks that toolchain, ordinary tests still run; use a suitable Linux builder
for the race run. Python Top-K tests and the cross-language integration command
remain documented in the root README.
