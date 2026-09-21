# Contract tests

The contract suites are Go code in [`internal/plugins/contract`](../../internal/plugins/contract), one per plugin type. The
same code runs in three places, so passing locally means passing in the platform:

1. **`llm-cache-plugin verify`** on your machine;
2. **the plugin manager** before it will activate a candidate (any failing check blocks activation);
3. **`go test ./internal/plugins/contract`**, which runs every SDK example against its suite and also checks that the suites
   *reject* 16 deliberately broken plugins (a suite that cannot fail proves nothing).

```bash
# test a plugin that is already running
llm-cache-plugin verify . --endpoint http://127.0.0.1:8080 [--token T] [--model NAME]

# build it, start it isolated in Docker, test it, clean up
llm-cache-plugin verify .

# JSON output for CI
llm-cache-plugin verify . --json
```

`run.sh` in this directory does the build-and-verify step from any checkout:

```bash
sdk/contract-tests/run.sh sdk/examples/queue
```

Suites, one per type (check names are listed in [docs/PLUGIN_CONTRACTS.md](../../docs/PLUGIN_CONTRACTS.md)):

| Type | Highlights |
| --- | --- |
| llm | reply, Unicode/64 KiB prompts, cancellation and deadlines, blank prompt is 4xx, concurrency |
| embedder | shape, finite, unit length, stable dimension, deterministic, model identity, concurrency |
| vector-store | ordering, `top_k`, inclusive threshold, wrong dimension, delete, **rebuild replaces**, concurrency, flush |
| persistence | round-trip of every field, not-found vs failure, pagination, idempotent delete, large/Unicode, concurrency |
| queue | durable enqueue, lease/visibility timeout/redelivery, stale-ack rejection, ack only after success, graceful shutdown |
| policy | lifecycle, victim does not delete, flush reusable, concurrent hooks, no lost events |
| embedding-model | plugin discovery + the embedder suite |
| vector-index | plugin discovery + the vector-store suite |
| similarity-metric | discovery, metric identity, FAISS compatibility, **lower-is-better** score conversion + the vector-store suite |

Stateful suites (vector store, persistence, queue) write to the plugin, so they refuse to run against a candidate that
already holds data. Point them only at a fresh instance.
