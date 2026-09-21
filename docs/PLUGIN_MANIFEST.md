# `plugin.yaml` manifest reference

Every container or Python plugin ships a `plugin.yaml` at the root of its repository (or
image, at `/plugin.yaml`). It is **data only**: strict, versioned, and closed-world. It can
describe a plugin but can never carry a command, a mount, a privilege or a secret value.
Validate one with `llm-cache-plugin manifest plugin.yaml`; the platform runs the same code.

## Example (container plugin)

```yaml
apiVersion: llmcache.dev/v1alpha1
kind: Plugin

metadata:
  name: my-plugin            # 1-63 chars: a-z 0-9 '-', starts/ends alphanumeric
  version: 1.0.0             # semantic version
  description: Example LLM Cache plugin

spec:
  type: llm                  # one of the nine types

  runtime:
    mode: container
    dockerfile: Dockerfile   # relative to the repository, no '..'
    context: .               # build context, relative, default "."
    port: 8080               # 1024-65535, the port the plugin listens on

  health:
    path: /health            # absolute path, no query
    timeout: 5s              # 100ms - 60s

  config:                    # non-secret configuration, entered in the UI
    properties:
      model:
        type: string
        required: true
      request_timeout:
        type: duration
        default: 30s

  secrets:                   # names only; values are entered separately, stored encrypted
    - name: API_TOKEN
      required: true

  resources:                 # optional; capped by PLUGIN_CPU_LIMIT / PLUGIN_MEMORY_LIMIT
    cpu: 500m
    memory: 256Mi

  network:
    egress: none             # default; "internet" only if the operator allows it
```

## Example (Python plugin)

```yaml
apiVersion: llmcache.dev/v1alpha1
kind: Plugin
metadata: {name: my-embedding-model, version: 1.0.0}
spec:
  type: embedding-model      # or vector-index, similarity-metric
  runtime:
    mode: python             # built into a platform-owned runner; no Dockerfile allowed
  python:
    module: my_model         # package (directory) or single module file, under `path`
    path: .                  # default "."
    backend: my-model        # the name your code passes to @register(...)
    requirements: requirements.txt   # optional extra pip requirements
  config:
    properties:
      dim: {type: integer, default: 384, min: 8, max: 4096}
```

The platform copies your package into the platform's own embedding (or vector-store)
service inside a runner image, selects your registered backend by environment, and starts
it isolated. Your package imports the platform registry (`from app.models import EmbeddingModel, register`),
which exists inside the runner.

## Fields

| Field | Required | Notes |
| --- | --- | --- |
| `apiVersion` | yes | must be `llmcache.dev/v1alpha1` |
| `kind` | yes | must be `Plugin` |
| `metadata.name` | yes | DNS-label style, max 63 |
| `metadata.version` | yes | semver `MAJOR.MINOR.PATCH[-pre][+build]` |
| `metadata.description` | no | max 500 chars |
| `spec.type` | yes | `llm embedder vector-store persistence queue policy embedding-model vector-index similarity-metric` |
| `spec.contract.version` | no | defaults to and must be `v1` |
| `spec.runtime.mode` | no | `container` (six Go types), `python` (three Python types), `endpoint` (hosted endpoint; no build inputs). Defaults by type. |
| `spec.runtime.dockerfile` / `context` | container | relative paths inside the repository; default `Dockerfile` / `.` |
| `spec.runtime.port` | container | 1024-65535, default 8080. Python runners use 8001 (embedding) / 8002 (vector) |
| `spec.python.module` | python | Python identifier |
| `spec.python.backend` | python | lowercase registry name |
| `spec.python.path` / `requirements` | no | relative paths |
| `spec.health.path` / `timeout` | no | defaults `/health`, `5s` |
| `spec.config.properties.<name>` | no | see below |
| `spec.secrets[]` | no | `name` (`^[A-Z][A-Z0-9_]{0,63}$`), `required`, `description` |
| `spec.resources` | no | `cpu` (`500m`/`0.5`, max 4), `memory` (`256Mi`, 16Mi-4Gi), `pids` (max 1024), `tmpfs` (1Mi-1Gi) |
| `spec.network.egress` | no | `none` (default) or `internet` |
| `spec.verify.vectorDim` | no | dimension hint for contract tests of vector stores that do not report one |

### Configuration properties

| `type` | Extra keys | Delivered as |
| --- | --- | --- |
| `string` | | text (max 4096) |
| `integer`, `number` | `min`, `max` | number |
| `boolean` | | true/false |
| `duration` | | Go duration string, e.g. `30s` |
| `enum` | `enum: [...]` | one of the values |

`required: true` properties cannot have a `default`. Property names match `^[a-z][a-zA-Z0-9_]{0,63}$`
and must not look like secrets (`token`, `password`, `api_key`, ...): declare those under `secrets`.
At runtime a container receives each value as `CONFIG_<NAME>` (upper-cased), for example
`dim` becomes `CONFIG_DIM`, and each secret under its declared name. A Python vector plugin
also receives `VECTOR_DIM` (the active embedder's dimension); a vector-index plugin may declare a
`metric` property (`cosine`/`euclidean`) to choose the built-in metric it runs with.

### Variables the platform owns

`PORT`, `PLUGIN_AUTH_TOKEN` (a per-instance bearer token your service should check on every route except
`/health`), `OMP_NUM_THREADS` and friends (sized to the CPU limit), and for Python runners
`EMBEDDING_MODEL_BACKEND`, `VECTOR_INDEX_BACKEND`, `SIMILARITY_METRIC`, `VECTOR_DIM`. A manifest cannot declare a secret with one of these
names, `PATH`, `HOME`, `LD_PRELOAD`, `LD_LIBRARY_PATH` or `PYTHONPATH`.

## What is rejected

Validation lists every problem at once. Unknown fields are errors, and these get a specific message
(matched at any depth and regardless of `snake_case`/`camelCase`):

| Rejected | Why |
| --- | --- |
| unsupported `apiVersion`, `kind`, `spec.contract.version` | only the versions this release speaks |
| unknown `spec.type`, unknown fields | closed world |
| invalid name / semantic version | |
| `privileged`, `allowPrivilegeEscalation` | privileged containers are never allowed |
| `hostNetwork`, `network_mode` | host networking is never allowed |
| `hostPID`, `pid`, `hostIPC`, `ipc` | host namespaces are never allowed |
| `volumes`, `mounts`, `binds`, `hostPath`, `devices`, `dockerSocket` | no host filesystem, devices or Docker socket |
| `capabilities`, `cap_add`, `cap_drop`, `securityContext`, `security_opt`, `sysctls`, `user`, `runAsUser` | all capabilities are dropped and the user is fixed non-root |
| `command`, `entrypoint`, `args` | the image's own entrypoint is used; no command injection |
| `env`, `environment`, `buildArgs` | configuration goes under `spec.config`, secrets under `spec.secrets`; **build arguments are refused so secrets can never reach a build** |
| absolute paths, `..`, backslashes, control characters in `dockerfile` / `context` / `python.path` / `requirements` | build paths must stay inside the repository |
| `port` < 1024, `health.path` with a scheme/host/query/`..` | |
| a credential-looking string anywhere (`sk-...`, `AIza...`, `ghp_...`, PEM keys, `Bearer <token>`, `user:pass@` URLs) | never embed a plaintext secret |
| a config property named like a secret | declare it as a secret instead |
| `cpu` > 4, `memory` outside 16Mi-4Gi, `pids` > 1024, `tmpfs` outside 1Mi-1Gi | unsafe resource requests; the operator's `PLUGIN_CPU_LIMIT`/`PLUGIN_MEMORY_LIMIT` are enforced on top |
| `mode: container` for a Python type (or `python` for a Go type) | wrong runtime for the type |

## Where to start

`sdk/manifests/<type>.plugin.yaml` has a valid template for every type, and
`sdk/examples/<type>/` has a working plugin with its manifest, Dockerfile (or Python package),
README, example configuration and expected responses.
