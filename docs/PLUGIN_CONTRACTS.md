# Plugin contracts (protocol v1)

A plugin is a small HTTP/JSON service. The orchestrator talks to it through a **versioned**
contract (`/v1/...`) and never through anything else. This file is the specification; the
contract test suites in `internal/plugins/contract` enforce it, and `llm-cache-plugin verify`
runs them on your machine. Every SDK example passes its suite.

## Common rules

* JSON in, JSON out, `Content-Type: application/json`. UTF-8.
* `GET /health` returns `200` when the plugin can serve. It must answer **without** credentials.
* If the platform started your container it set `PLUGIN_AUTH_TOKEN`: require
  `Authorization: Bearer <token>` on every route except `/health` (constant-time compare).
  A hosted endpoint receives the optional `ENDPOINT_TOKEN` the administrator entered as the same header.
* Errors: a non-2xx status. Preferably `{"error":{"code":"machine_code","message":"short text"}}`;
  `{"error":"text"}` is accepted too. **Only the status and the `code` are surfaced**; the body text is
  never copied into logs or responses because it could carry anything.
* The client never follows redirects, ignores proxy environment variables, and refuses
  responses over the limits below. Do not redirect.
* Client errors (bad input) must be `4xx`. A `5xx`, a hang, or `200` for invalid input fails the suite.
* Requests are timed out (default 10 s; LLM 60 s or your `request_timeout`; vector rebuild 30 s).
  Honour cancellation.
* IDs (`entry id`, `job id`, victim ids) are **stable, non-blank and safe as one URL path segment**:
  `^[A-Za-z0-9._~-]{1,128}$`, not `.` or `..`.

## `llm`

```
POST /v1/complete   {"model":"my-model","prompt":"hello"}   ->  200 {"reply":"answer"}
GET  /v1/usage      (optional)                               ->  200 usage JSON | 404
GET  /health
```

* `reply` must be non-blank and at most 1 MiB. Prompts up to 64 KiB (and beyond) with Unicode must work.
* `model` is the administrator's `model` setting. Blank prompts are a `4xx`.
* `/v1/usage` may return `{"model":"x","requests":12,"tokens":3400,"exhausted":false,"resets_in_seconds":900,
  "request_limit":20,"requests_left":8}` and appears in `/stats` and the UI.
* Concurrent requests must be safe. Never log tokens, provider error bodies or prompts.

Suite: `health`, `reply`, `unicode_and_large_prompt`, `canceled_context`, `expired_deadline`,
`malformed_request_rejected`, `concurrent_requests`, `usage_optional`.

## `embedder` (and Python `embedding-model`)

```
POST /v1/embed        {"text":"hello"}  ->  200 {"vector":[...],"dim":384,"model":"model-name"}
GET  /v1/model-info                     ->  200 {"name":"model-name","dim":384}
GET  /health
```

* `len(vector) == dim`, every value finite, vectors **unit length** (the cache measures cosine distance).
* Deterministic for the same text (identical prompts must hit), constant `dim`, `model` equal to `name`.
* `name` + `dim` identify the vector space. Changing either later needs an administrator to
  **flush or re-embed** the cache; the platform never mixes spaces silently.

Suite: `health`, `model_info`, `vector_shape_and_finite`, `stable_dimension`, `normalized_unit_length`,
`deterministic_for_same_text`, `concurrent_calls`, `malformed_request_rejected`.

## `vector-store` (and Python `vector-index`, `similarity-metric`)

```
POST   /v1/search   {"vector":[...],"top_k":3,"threshold":0.25}  ->  {"matches":[{"id":"a","distance":0.02}]}
POST   /v1/upsert   {"vector":[...]}                              ->  {"id":"generated-id"}
DELETE /v1/entries/{id}                                            ->  200 | 404
GET    /v1/size                                                    ->  {"size":42}
POST   /v1/flush                                                   ->  200
POST   /v1/rebuild  {"entries":[{"id":"a","vector":[...]}]}       ->  {"restored":1}
GET    /v1/info     (optional)                                     ->  {"dim":384,"metric":"cosine"}
GET    /health
```

Semantics (unchanged from the built-in FAISS service):

* **Lower distance means more similar.** A hit requires `distance <= threshold` (inclusive).
* Results are best-to-worst and at most `top_k`; a negative threshold matches nothing.
* `upsert` returns a new stable id; ids of `rebuild` are the caller's (the cache's persisted ids).
* **`rebuild` replaces all contents**; it never merges. `rebuild` with `[]` empties the store.
* Wrong-dimension vectors and `top_k < 1` are `4xx`. Deleting an unknown id is `404`.
* `/v1/info` (or extra `dim`/`metric` fields on `/health`) lets the platform refuse an embedder whose
  vectors do not fit and detect a metric change (which needs a confirmed threshold).
* Every operation must be safe under concurrent requests (the Python runner serialises index calls for you).

Suite: `health`, `dimension`, `flush_empties_store`, `upsert_returns_unique_safe_ids`,
`nearest_is_identical_vector`, `top_k_and_best_to_worst_order`, `threshold_is_inclusive`,
`wrong_dimension_rejected`, `invalid_top_k_rejected`, `delete_semantics`, `rebuild_replaces_contents`,
`rebuild_rejects_wrong_dimension`, `concurrent_upserts_and_searches`, `usable_after_flush`.
The suite writes to and flushes the candidate, so it refuses a non-empty store.

### Python runners

The three Python types run inside a runner image and additionally expose introspection used to
prove your package was discovered and selected:

```
GET  /v1/runner-info     {"kind":"embedding|vector","backend":"...","metric":"...","registered_backends":[...],"registered_metrics":[...]}
GET  /v1/metric-info     {"name":"sdk-angular","faiss_metric_type":0}
POST /v1/metric/distance {"scores":[-1,0,0.5,1]} -> {"distances":[...]}
```

* **Embedding model**: subclass `EmbeddingModel` (`name`, `dim`, `embed`) and `@register("my-model")`.
* **Vector index**: subclass `VectorIndex` and `@register("my-index")`; it is handed `(dim, metric)`.
* **Similarity metric**: subclass `SimilarityMetric` (`name`, `faiss_metric_type`, `to_distance`) and
  `@register("my-metric")`. Only FAISS inner-product (`0`) and L2 (`1`) scores are supported. `to_distance`
  runs in-process inside the FAISS search; nothing crosses the network per score. Suite additions:
  `plugin_discovery`, `metric_identity`, `compatible_with_faiss_index`, `score_conversion_lower_is_better`
  (inner-product distances must not rise as scores rise; L2 distances must not fall), then the full vector-store suite.

## `persistence`

```
PUT    /v1/entries/{id}   {"id","prompt","reply","vector":[...],"created_at":"RFC3339"}   ->  200|201|204
GET    /v1/entries/{id}   ->  200 entry | 404 {"error":{"code":"not_found"}} | 422 {"error":{"code":"invalid_data"}}
GET    /v1/entries?cursor=&limit=   ->  {"entries":[...],"next_cursor":""}
DELETE /v1/entries/{id}   ->  200|204   (deleting an unknown id is not an error)
GET    /v1/size           ->  {"size":N}
POST   /v1/flush          ->  200|204
GET    /health
```

* **Persist every field.** The vector store and the eviction policy are rebuilt from these entries
  (`created_at` orders the policy replay), so a lossy store silently breaks recovery.
* Three failure classes are distinct: **not found** (`404 not_found`), **invalid stored data**
  (`422 invalid_data`), and **backend failure** (`5xx`/unreachable). The adapter maps them to
  `ErrNotFound`, `ErrInvalidData` and `ErrBackend`.
* `list` pages must terminate (a cursor that does not advance is an error) and return each entry once.
* Durable storage is your responsibility: the SDK example keeps entries in memory only.

Suite: `health`, `size_at_start`, `save_load_roundtrip_all_fields`, `overwrite_updates_entry`,
`not_found_is_distinct_from_failure`, `unicode_and_large_entries`, `url_safe_id_characters`, `list_and_size`,
`list_pagination`, `delete_is_idempotent`, `mismatched_id_rejected`, `concurrent_saves`, `flush_clears_store`
(only when the store started empty; it never flushes other data).

## `queue` (lease based)

The Go interface passes a callback to `Run(ctx, handler)`, which cannot cross a process boundary,
so a remote queue is a **lease protocol**:

```
POST /v1/jobs               {"prompt","reply","vector":[...]}           ->  201 {"id":"job-id"}     (durable, or an error)
POST /v1/jobs/lease         {"max_jobs":1,"visibility_timeout_seconds":60,"wait_seconds":2}
                                                                          ->  200 {"jobs":[{"id","prompt","reply","vector","lease_token","attempt"}]} | 204
POST /v1/jobs/{id}/ack      {"lease_token":"t"}                          ->  200 | 409 (lease lost) | 404
POST /v1/jobs/{id}/nack     {"lease_token":"t"}                          ->  200 | 409 | 404          (visible again at once)
GET  /v1/jobs/stats         (optional)                                   ->  {"pending":N,"leased":M}
GET  /health
```

The Go adapter leases a job, calls the existing cache-update handler, **acknowledges only after the
handler succeeds**, and nacks on failure (a panic counts as a failure). Required semantics:

* `201` only once the job is stored durably; otherwise an error.
* A leased job is invisible until the visibility timeout, then redelivered with a new token and a higher `attempt`.
* Acknowledging with a stale or wrong token is `409` and must not delete the job.
* Long-poll `wait_seconds` (up to 20) is allowed; honour client disconnects.
* Delivery is at-least-once: a lost ack redelivers, so the cache-update path must tolerate a repeat.

Suite: `health`, `queue_starts_empty`, `enqueue_is_durable_and_lease_returns_payload`,
`visibility_timeout_redelivers_and_stale_ack_rejected`, `nack_makes_job_visible_immediately`,
`wrong_token_and_unknown_job`, `adapter_acks_only_after_handler_success`, `run_stops_promptly_when_idle`,
`graceful_shutdown_finishes_inflight_job`, `concurrent_consumers_process_each_job_once`, `stats_optional`.

## `policy`

```
GET  /v1/policy   ->  {"name":"my-policy"}
POST /v1/events   {"events":[{"op":"hit|insert|delete","id":"a"}, ...]}   ->  200     (applied in order)
GET  /v1/victim   ->  {"id":"a","ok":true} | {"ok":false}                  (selects; never deletes)
POST /v1/flush    ->  200                                                  (the policy stays usable)
GET  /health
```

* **Victim selects but does not delete**; the orchestrator deletes the entry and then sends `delete`.
* Unknown or repeated `delete`/`hit` are harmless; a repeated `insert` must not duplicate state.
* `flush` leaves the policy reusable; requests may be concurrent.

**Latency trade-off.** Every hit, insert and delete would otherwise cost a network round trip on the
query path. The adapter therefore sends events from a background goroutine, **in order and in batches**
(`events` is an array for exactly this reason), and `Victim`/`Flush` first wait until all earlier events
were delivered. The price: a hit is applied by the plugin slightly after the response is sent, and a plugin
outage can drop hit notifications (counted and reported through health). Choose a remote policy only
if that is acceptable; the built-in LRU/LFU/FIFO have none of this overhead.

Suite: `health_and_name`, `lifecycle`, `flush_leaves_policy_reusable`, `victim_does_not_delete`,
`concurrent_hooks`, `no_events_lost`.

## Limits

| | |
| --- | --- |
| LLM reply | 1 MiB |
| Embedding | up to 8192 components; response 4 MiB |
| Vector store response | 4 MiB; up to 2 000 000 rebuild entries |
| Persistence response | 32 MiB per page; pages of 500 |
| Queue response | 16 MiB |
| Anything else | 8 MiB |
| Controller proxy | requests 64 MiB, responses 32 MiB |

## Versioning

`spec.contract.version: v1` is the only version. Additions inside v1 are optional endpoints or fields
(for example `/v1/info`, `/v1/usage`, `/v1/jobs/stats`); required behaviour never changes within a version.
