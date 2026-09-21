# Plugin platform

The plugin platform lets an administrator add or replace any pluggable component of
the LLM Cache from the web UI, with no source edits, no `.env` edits, no Compose edits
and no rebuild. It is **additive and off by default**: with `ENABLE_PLUGIN_INSTALLATION`
unset the orchestrator behaves exactly as before and none of the new endpoints exist.

| Document | Read it for |
| --- | --- |
| this file | architecture, what works, what needs container infrastructure, limitations |
| [PLUGIN_MANIFEST.md](PLUGIN_MANIFEST.md) | the `plugin.yaml` schema and what is rejected |
| [PLUGIN_CONTRACTS.md](PLUGIN_CONTRACTS.md) | the versioned `/v1` protocol of every plugin type and its contract tests |
| [PLUGIN_SECURITY.md](PLUGIN_SECURITY.md) | threat model, isolation, secrets, the Docker-socket warning |
| [PLUGIN_OPERATIONS.md](PLUGIN_OPERATIONS.md) | settings, admin API, lifecycle, per-component activation rules, runbooks |
| [`sdk/`](../sdk/README.md) | copy-and-edit examples for all nine types and the `llm-cache-plugin` tool |

## The nine plugin types

| Type | Contract | Runs as | Activated into |
| --- | --- | --- | --- |
| `llm` | `llm.Backend` | isolated service | LLM slot |
| `embedder` | `embedder.Embedder` | isolated service | embedder slot |
| `vector-store` | `vectorstore.VectorStore` | isolated service | vector-store slot |
| `persistence` | `persistence.Store` | isolated service | persistence slot |
| `queue` | `cachequeue.Queue` | isolated service | queue slot |
| `policy` | `policy.EvictionPolicy` | isolated service | policy slot |
| `embedding-model` | Python `EmbeddingModel` | **runner image**: the embedding service + your package | embedder slot |
| `vector-index` | Python `VectorIndex` | **runner image**: the vector-store service + your package | vector-store slot |
| `similarity-metric` | Python `SimilarityMetric` | the same vector runner (no per-score network call) | vector-store slot |

All the existing implementations stay: stub/OpenAI/Gemini/example-http LLMs, the HTTP
embedder and vector-store clients, Redis and memory persistence, the Redis queue,
LRU/LFU/FIFO, sentence-transformers, FAISS, cosine and Euclidean, the environment
selectors and the Python `app/plugins/` auto-discovery. They are the *built-ins* a slot
returns to when its plugin is deactivated.

## Architecture: control plane and data plane

```
 browser (Plugins page)
        |  Authorization: Bearer ADMIN_TOKEN
        v
+--------------------------- orchestrator process (Go) ----------------------------+
|  admin API  ->  plugin manager  ->  registry (Redis, llm_cache_plugins:*)         |
|                     |  encrypted secrets (AES-256-GCM, PLUGIN_SECRET_KEY)         |
|                     |  manifest validation, state machine, audit, contract tests  |
|  query path  ->  dynamic adapters (atomic pointer per component) ->  built-in     |
|                                                                   or remote adapter|
+---------|-----------------------------------------------------|-------------------+
          | hosted endpoint: https, SSRF-guarded                | controller API (bearer)
          v                                                     v
   developer's own service                        plugin-controller (internal service)
                                                        |  docker CLI via docker-socket-proxy
                                                        v
                                            isolated plugin containers
                                            (read-only, non-root, no caps, limits,
                                             private network, secrets at start only)
```

* **Control plane** - the plugin manager inside the orchestrator: registry, manifest
  validation, installation state, encrypted secrets, contract verification, activation,
  health monitoring, upgrade, rollback, deletion, audit. Exposed through
  authenticated `/admin/...` routes and the `/plugins` page.
* **Data plane** - developer code only ever runs in **separate** processes or
  containers. It never runs in the orchestrator, the embedding service or the vector-store
  service, and nothing is copied into a running service. There is no Go `plugin` package,
  no shell command from an API request, and no upload into a live container.
* **Plugin controller** (`cmd/plugin-controller`) - the only component that touches the
  container runtime. It fetches exact Git revisions, builds images, pulls and pins
  digests, scans, starts hardened instances, health-checks them, proxies the
  orchestrator's calls, and keeps sanitised logs. It is internal-only and authenticated,
  and its interface (`internal/plugins/ctlapi`) is runtime-agnostic so a Kubernetes
  implementation can replace the local Docker one.

Dynamic activation follows one path for every component:

```
old plugin keeps serving -> prepare candidate -> contract tests -> health check
   -> migrate / rebuild / replay state (cache writes paused, queries keep flowing)
   -> atomic switch -> monitor -> old instance removed after the rollback window
```

A failed verification, migration or activation leaves the previous implementation
active. Requests already running finish on the old implementation; requests that start
after the switch use the new one.

## What works now

Everything in this table is implemented and covered by an automated test that was run
(see [Validation](#validation-commands) for exact commands and the results reported with
each release).

| Capability | Status |
| --- | --- |
| Strict versioned `plugin.yaml`; all nine types; rejects privileged/host-network/host-PID/mounts/socket/capabilities/commands/build-args/secret values/unsafe paths and resources | works, unit-tested |
| Encrypted secrets, unique nonces, wrong-key error, never returned by any API or log | works, unit- and e2e-tested |
| Persistent registry in a separate Redis namespace + in-memory implementation | works (Redis tested against a real Redis) |
| Authenticated admin API, strict JSON, size limits, disabled without `ADMIN_TOKEN` | works, tested |
| Hosted-endpoint plugins for all six Go types, SSRF/redirect/metadata protection | works, tested against real processes |
| Versioned remote protocols and adapters for llm, embedder, vector-store, persistence, queue (lease based), policy (batched) | works, tested incl. malformed/oversized/timeout/failure injection |
| Reusable contract suites for all nine types; every SDK example passes its suite; 16 injected faults are caught | works |
| Atomic switching for every component, concurrent-activation protection, startup restoration | works (`go test -race`) |
| Component-specific rules: model-change confirmation with **flush or re-embed**, vector rebuild + verify, persistence **migrate / empty / adopt**, queue drain, policy replay, threshold confirmation for metrics | works, tested |
| Upgrade, rollback (restores previous threshold), delete restrictions | works, tested |
| Prebuilt-image install: digest pinning, resource-limited isolated start, health check | works against a real Docker daemon |
| GitHub-repository install: exact commit, size/symlink checks, isolated build, provenance | works against a real Docker daemon (local git in tests; `https://github.com/...` in production) |
| Python plugins built into specialised runner images; discovery, config, backend selection verified | works against a real Docker daemon (all three types) |
| Plugins web page: every state, verify/install/activate/deactivate/upgrade/roll back/delete/logs, confirmation dialogs | works, tested statically; exercised against the real API |
| `llm-cache-plugin verify` developer tool | works |
| End-to-end acceptance against the real compose stack | `scripts/e2e_plugins.sh` (see the release report for its last result) |

## What requires container infrastructure

* **Hosted endpoints need nothing extra** - only `ENABLE_PLUGIN_INSTALLATION=true`,
  `ADMIN_TOKEN` and `PLUGIN_SECRET_KEY`.
* **Prebuilt images, GitHub repositories and every Python plugin type need the plugin
  controller**, which needs a Docker daemon: `docker compose --profile plugins up -d`.
  A Docker daemon is effectively root on its host - read
  [PLUGIN_SECURITY.md](PLUGIN_SECURITY.md#the-docker-socket) before enabling it. Image
  and repository installation are therefore disabled until `PLUGIN_CONTROLLER_URL` is set.

## Local development vs production

| | Local development | Production |
| --- | --- | --- |
| Endpoint URLs | `ALLOW_INSECURE_PLUGIN_ENDPOINTS=true` allows http, loopback, private addresses, compose service names (cloud-metadata targets stay blocked) | default: https only, public addresses only, no redirects, no credentials/query in the URL |
| Local git / images | `PLUGIN_ALLOW_LOCAL_GIT`, `PLUGIN_ALLOW_LOCAL_IMAGES` on the controller | leave off: only `https://github.com/<owner>/<repo>` and registry images with a digest |
| Egress from plugins | none by default; `PLUGIN_ALLOW_EGRESS` lets a plugin ask for internet | keep off unless a plugin genuinely needs it |
| Scanning | skipped (recorded as `skipped`, never as `passed`) | `PLUGIN_SCANNER=trivy` and `PLUGIN_REQUIRE_SCAN=true` (bring a trivy binary into the controller image) |
| Docker access | the compose socket proxy | a dedicated build/plugin host or rootless Docker; see the security doc |
| Registry backend | `PLUGIN_REGISTRY_BACKEND=memory` is allowed for experiments | Redis with persistence (the default) |

## Security assumptions

* The administrator holding `ADMIN_TOKEN` is trusted. There is one admin role.
* Plugin code is **not** trusted: it is isolated and its output is validated, but a
  malicious LLM plugin can of course return misleading text, and a malicious
  persistence plugin can return wrong data. Only install plugins you have reviewed.
* Builds execute the plugin's own Dockerfile / `pip install` in the Docker daemon's
  builder. That is arbitrary code execution inside a build sandbox that is **not** rootless
  BuildKit (see limitations). Build only repositories you trust.
* Redis is trusted infrastructure on a private network.

## Known limitations

Be explicit about these before relying on the platform:

1. **The builder is the Docker daemon's, not rootless BuildKit.** Rootless BuildKit
   isolation is *not implemented*. Repository builds run in the daemon's builder without
   build arguments or secrets, but a hostile Dockerfile still executes with network access
   in that builder.
2. **A Docker daemon is root-equivalent.** The socket proxy narrows the API (no exec,
   volumes, secrets, swarm) but container creation stays powerful. The controller must be
   treated as a privileged, internal-only component.
3. **Vulnerability scanning is optional and needs a scanner you provide** (`trivy`). Without
   it the scan is recorded as `skipped`; with `PLUGIN_REQUIRE_SCAN=true` that blocks the install.
4. **No per-host egress allow-list.** A plugin has either no network egress (default) or
   ordinary internet egress if the operator allows it. In *publish* mode (controller on the
   host, development only) egress is not restricted at all.
5. **One orchestrator instance.** Activations are serialised in-process; running several
   orchestrators against one registry is not supported.
6. **No automatic rollback on degraded health.** Health is monitored and shown; a rollback
   is an administrator action.
7. **Recency/frequency metadata is not persisted**, so a new eviction policy starts from the
   persisted insertion order (the activation result says so).
8. **Cache writes pause while state is migrated or rebuilt.** Queries keep being served, but
   writes wait; for a very large cache a persistence migration or vector rebuild can take a
   while. Re-embedding is limited to `MaxReembedEntries` (50 000) - beyond that, flush.
9. **Persistence plugins that keep state in memory** (like the SDK example) lose it on restart;
   real plugins should use a database. There is no platform-managed volume yet.
10. **Only one Python plugin per vector runner**: a custom index and a custom metric cannot be
    combined in one runner yet (a custom metric runs with the built-in FAISS index, a custom
    index runs with a built-in metric).
11. **Only hosted-endpoint plugins are supported by the *Go-side* built-in LLM fallback
    wrapper**: activating an LLM plugin replaces the whole LLM slot, including
    `LLM_FALLBACK_MODE`.
12. **One Docker network per plugin.** Docker's default address pools allow only about 30 user-defined networks in
    total (for all your stacks). Removing a plugin removes its network, but many simultaneous plugins - or leftovers from
    other stacks - can exhaust the pool ("all predefined address pools have been fully subnetted"). Raise
    `default-address-pools` in the daemon configuration if you need more.
13. Roles, per-user audit identity, mutual TLS between services and multi-tenancy are not part
    of this release. The audit log records the action, plugin and outcome under one `admin` actor.

## Validation commands

```bash
export PATH=$HOME/go/bin:$PATH        # if Go is installed there

go test ./...
go vet ./...
go test -race ./...

# Python services and plugin runners (inside a container that has the dependencies)
docker build -t llmcache-pytest -f deploy/python-test.Dockerfile .
docker run --rm -v "$PWD":/repo -w /repo/embedding_service    llmcache-pytest python -m unittest discover -s tests -v
docker run --rm -v "$PWD":/repo -w /repo/vector_store_service  llmcache-pytest python -m unittest discover -s tests -v

python scripts/test_topk_integration.py   # needs faiss/fastapi/uvicorn on the host python
python scripts/benchmark/selftest.py
git diff --check

# plugin-specific
go test ./internal/plugins/...                        # manifest, secrets, registry, protocols, contracts, manager, admin
go test ./internal/plugins/contract -run SDK          # every SDK example against its suite
go test ./internal/plugins/contract -run Broken       # the suites reject 16 injected faults
go test -race ./internal/plugins/...

# real Docker (builds images, starts hardened containers, runs suites through the proxy)
LLMCACHE_DOCKER_TESTS=1 go test -timeout 30m ./internal/plugins/controller -run Docker -v

# compose configuration
docker compose config -q
docker compose --profile plugins config -q
docker compose -f docker-compose.prod.yml config -q

# full end-to-end acceptance against the real stack (long; builds images)
scripts/e2e_plugins.sh
```

If Docker, Redis or another tool is missing, the corresponding tests skip and say so; a
skipped test is not a passed test.
