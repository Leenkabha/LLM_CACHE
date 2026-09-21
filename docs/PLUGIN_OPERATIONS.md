# Operating the plugin platform

## Turn it on

Nothing changes until you opt in. Management is enabled only when **all three** are set:

| Setting | Meaning |
| --- | --- |
| `ENABLE_PLUGIN_INSTALLATION=true` | master switch (default `false`) |
| `ADMIN_TOKEN` | protects `/admin/...` (and `/flush`, `/policy`); non-empty or the API stays disabled |
| `PLUGIN_SECRET_KEY` | base64 of 32 random bytes: `openssl rand -base64 32`; encrypts plugin secrets |

Startup fails, with a message naming the missing setting, if you enable installation without a valid token and key.
Then open **`/plugins`** in the web UI (a "Plugins" link appears on the query page), enter the admin token, and use it.

### Settings

| Setting | Default | Purpose |
| --- | --- | --- |
| `ENABLE_PLUGIN_INSTALLATION` | `false` | master switch |
| `PLUGIN_SECRET_KEY` | | master encryption key (never commit it) |
| `ALLOW_INSECURE_PLUGIN_ENDPOINTS` | `false` | development only: http, loopback, private addresses, service names (also lets the manager accept absolute local repository paths, which the controller separately refuses unless `PLUGIN_ALLOW_LOCAL_GIT`) |
| `PLUGIN_CONTROLLER_URL` | empty | internal controller; empty disables image/repository installs |
| `PLUGIN_CONTROLLER_TOKEN` | | shared secret with the controller, at least 16 characters |
| `PLUGIN_BUILD_TIMEOUT` | `10m` | build/pull limit |
| `PLUGIN_HEALTH_TIMEOUT` | `60s` | wait for a candidate to become healthy |
| `PLUGIN_CPU_LIMIT` | `1` | default **and ceiling** CPUs per plugin (`500m`, `2`) |
| `PLUGIN_MEMORY_LIMIT` | `512Mi` | default **and ceiling** memory per plugin |
| `PLUGIN_REGISTRY_BACKEND` | `redis` | `redis` or `memory` (records lost on restart) |
| `PLUGIN_ROLLBACK_WINDOW` | `10m` | how long a replaced plugin's container is kept before it is stopped |

Controller-only settings (set on the `plugin-controller` service): `PLUGIN_RUNNER_DIR`, `PLUGIN_MAX_IMAGE_BYTES`
(2 GiB), `PLUGIN_MAX_SOURCE_BYTES` (100 MiB), `PLUGIN_SCANNER` (`none`/`trivy`), `PLUGIN_SCAN_FAIL_ON`
(`CRITICAL,HIGH`), `PLUGIN_REQUIRE_SCAN`, `PLUGIN_ALLOW_EGRESS`, `PLUGIN_ALLOW_LOCAL_IMAGES`, `PLUGIN_ALLOW_LOCAL_GIT`,
`PLUGIN_CONTROLLER_PUBLISH`, `PLUGIN_CONTROLLER_CONTAINER`, `PLUGIN_CONTROLLER_ADDR`.

### Hosted endpoints only (no Docker)

```bash
# .env
ENABLE_PLUGIN_INSTALLATION=true
ADMIN_TOKEN=$(openssl rand -hex 24)
PLUGIN_SECRET_KEY=$(openssl rand -base64 32)
docker compose up --build -d
# open http://localhost:8080/plugins
```

### With image and repository installs

```bash
# .env (in addition)
PLUGIN_CONTROLLER_URL=http://plugin-controller:8090
PLUGIN_CONTROLLER_TOKEN=$(openssl rand -hex 24)
docker compose --profile plugins up --build -d
```

Production: add the same values to `production.env` and run
`docker compose -f docker-compose.prod.yml --env-file production.env --profile plugins up --build -d`.
Read [PLUGIN_SECURITY.md](PLUGIN_SECURITY.md#the-docker-socket) first.

## Admin API

All routes need `Authorization: Bearer $ADMIN_TOKEN`. Bodies are strict JSON. Nothing returned contains a secret,
an encrypted blob, an authorization header, a private prompt or reply, or a URL query string.

| Route | Purpose |
| --- | --- |
| `GET /admin/plugins` | all plugins, the nine components with their active implementation, settings |
| `GET /admin/plugins/{id}` | one plugin (state, health, source, digest, commit, scan, provenance, contract results; secrets shown as names + "configured") |
| `POST /admin/plugins/verify` | dry run: validate, resolve the manifest, run the contract suite for a hosted endpoint; persists nothing |
| `POST /admin/plugins/install` | create the record and start verification in the background (`202`) |
| `POST /admin/plugins/{id}/activate` | activate a verified plugin (`{"confirmations":{...}}`) |
| `POST /admin/plugins/{id}/deactivate` | return the component to its built-in implementation |
| `POST /admin/plugins/{id}/upgrade` | install a newer version as a separate record (same name and type, higher version) |
| `POST /admin/plugins/{id}/rollback` | make the previous plugin (or the built-in) active again |
| `DELETE /admin/plugins/{id}` | delete an inactive plugin: stops its container, removes secrets and record |
| `GET /admin/plugins/{id}/logs?limit=` | sanitised log lines |
| `GET /admin/audit?plugin=&limit=` | audit events |

Install body (all sources): `type`, `mode` (`endpoint`/`image`/`repository`), `config`, `secrets` (write-only), optional
`manifest` (plugin.yaml text), `activate`, `confirmations`; plus `endpoint` (+ `name`, `version`), `image`, or `repo` (+ `revision`).
A hosted endpoint accepts one secret, `ENDPOINT_TOKEN`, sent as `Authorization: Bearer`.

Status codes: `202` accepted, `400` invalid request, `401` bad token, `404` unknown, `409` **confirmation needed**
(`{"error": "...", "requires": {"field": "...", "options": [...], "warning": "..."}}`) or busy, `422` incompatible,
`429` too many failed logins, `503` disabled.

## Lifecycle

States shown in the UI (the actual backend state): **Draft, Validating manifest, Testing contract, Building, Scanning,
Starting candidate, Migrating or rebuilding, Checking health, Activating, Verified, Active, Inactive, Failed, Rolling back,
Rolled back.** Allowed transitions are enforced (`plugins.ValidTransition`).

* **Install** -> validate manifest and configuration -> (image/repo) prepare: build or pull, scan, pin the digest -> start the
  isolated candidate -> run the contract suite -> health check -> **Verified**. A failure anywhere stops the candidate and
  leaves the plugin **Failed** with a sanitised reason; nothing else is affected.
* **Activate** serialises per component (a second concurrent activation of the same component gets `409`).
* **Deactivate** and **Rollback** use the same path, so they run the same compatibility checks.
* **Upgrade** keeps the old version running; activating the upgrade records the old one as the rollback target.
* **Delete** is refused for the active plugin and for the rollback target of an active plugin.
* A replaced plugin's container keeps running for `PLUGIN_ROLLBACK_WINDOW`, then stops; rolling back later restarts it on demand.
* **Startup**: the persisted selection is restored *before* the orchestrator rebuilds its state. A state-bearing component
  (embedder, vector store, persistence) that cannot be restored stops startup with a clear error rather than serving a
  different cache; a stateless one (LLM, queue, policy) falls back to the built-in and the plugin is marked failed. A wrong
  `PLUGIN_SECRET_KEY` is reported by name. Startup retries an unreachable controller for up to two minutes.

## What activation requires, per component

The manager compares the candidate with what is running and asks for an explicit decision instead of guessing. `409` responses
carry the options; the UI shows them in a dialog.

| Component | Activation |
| --- | --- |
| **LLM** | hot swap; cached replies stay valid. The built-in provider and `LLM_FALLBACK_MODE` are bypassed while a plugin is active. |
| **Embedder / embedding model** | Compares model **name and dimension** with the running one. Same identity: no action. Different, with a non-empty cache: you must choose `embedder_compat` = **`flush`** (delete the cache) or **`reembed`** (recompute every vector from the stored prompts with the new model - implemented, limited to 50 000 entries). Never mixes vector spaces. During a re-embed queries are answered as misses so a half-converted cache cannot mis-hit. A dimension that does not fit the active vector store is refused (`422`) - activate a matching vector store first. |
| **Vector store / vector index** | Cache writes pause; the candidate is **rebuilt from persistence**, its restored count and size are checked, and a stored vector must find itself. Only then is it switched in. Persisted vectors of the wrong dimension need `vector_compat` = `flush`. A failed rebuild flushes the candidate and leaves the old store serving. |
| **Similarity metric** (or a store whose metric differs) | Requires `threshold` - the scale differs between metrics (cosine is 0-2, Euclidean is unbounded), so a wrong threshold gives wrong hits or none. The previous threshold is restored on rollback/deactivate. |
| **Persistence** | Cache entries are never silently abandoned. If the cache has entries you choose `persistence_compat`: **`migrate`** (copy every entry, check the count, verify each; the candidate must be empty; the old store is left untouched), **`empty`** (start with an empty cache; the old entries stay in the old store), or **`adopt`** (use the candidate's own existing entries). A failed migration removes what it copied and leaves the old store active. |
| **Queue** | The new queue starts consuming and receives new jobs; the old queue's worker keeps running until it is empty (or 30 s), an in-flight job is always finished and acknowledged before its worker stops, and jobs left in the old queue stay stored there. Jobs are acknowledged only after the cache update succeeds. |
| **Policy** | Cache writes pause; persisted entries are replayed in creation order; the result must be consistent (victim is a persisted entry, or none for an empty cache). Recency/frequency history is not persisted, and the activation result says so. |

## Runbooks

* **A plugin is stuck "Building"/"Starting candidate"**: `GET /admin/plugins/{id}/logs`, then the controller's logs
  (`docker compose logs plugin-controller`). Builds are capped by `PLUGIN_BUILD_TIMEOUT`.
* **"cannot restore the active ... plugin"** at startup: the message names the plugin. Fix the plugin or controller, or
  remove the selection with `redis-cli HDEL llm_cache_plugins:active <slot>` (`llm`, `embedder`, `vector-store`, `persistence`,
  `queue`, `policy`) to fall back to the built-ins (then `persistence`/`vector-store` state is rebuilt from the built-in store).
* **"all predefined address pools have been fully subnetted"** when starting a plugin: Docker has no address space left for another
  network (each plugin uses one). Remove unused networks (`docker network prune` after stopping stale stacks; plugin networks are
  named `llmcache-plugin-<id>` and labelled `llmcache.plugin=true`) or enlarge `default-address-pools` in Docker's `daemon.json`.
* **"encrypted with a different PLUGIN_SECRET_KEY"**: restore the original key. If it is gone, delete and reinstall the
  affected plugins and re-enter their secrets.
* **Roll back a bad change**: `POST /admin/plugins/{id}/rollback` (or **Roll back** in the UI). Rolling back a component whose
  cache state changed asks for the same confirmations as any activation.
* **Rotate a plugin secret**: upgrade the plugin (secrets you do not re-enter are carried over; one you enter replaces it).
* **Look at what happened**: `GET /admin/audit`; every install, verify, activation, rollback and delete is recorded (failures
  and denials too) with a scrubbed detail.
* **Start over**: deactivate and delete plugins from the UI; the built-ins are never touched.

## Files that matter

`internal/plugins/` (manifest, secrets, registry, safehttp, protocol, contract, dynamic, manager, admin, ctlapi, controller,
setup), `cmd/plugin-controller`, `cmd/llm-cache-plugin`, `internal/orchestrator/web/plugins.html`, `sdk/`, `deploy/controller.Dockerfile`,
`docker-compose.e2e.yml`, `scripts/e2e_plugins.sh`, `test/e2e`.
