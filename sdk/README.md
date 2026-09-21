# LLM Cache plugin SDK

Everything you need to write a plugin for the LLM Cache, install it from the web UI, and
prove it works. **Copy the example for your component, replace its implementation, push
the repository to GitHub, and install it from the UI** - you never edit the LLM Cache repository.

```
sdk/
  manifests/        one valid plugin.yaml template per type + the JSON Schema
  contract-tests/   how to run the contract suites (the same ones that gate activation)
  examples/
    llm/  embedder/  vector-store/  persistence/  queue/  policy/          Go, stdlib only, one Dockerfile each
    embedding-model/  vector-index/  similarity-metric/                    Python packages, built into a platform runner
```

Each example directory has a minimal working implementation, `plugin.yaml`, a Dockerfile (Go) or Python
package configuration, a README, `config.example.json` (an install request) and `expected-responses.md`.
Every example passes its matching contract suite; `go test ./internal/plugins/contract` proves it in CI.

## Five steps

1. **Pick the type** and copy `sdk/examples/<type>/` into a new repository.
2. **Replace the implementation.** Go plugins implement a small HTTP protocol (see
   [PLUGIN_CONTRACTS.md](../docs/PLUGIN_CONTRACTS.md)); Python plugins subclass the platform's `EmbeddingModel`,
   `VectorIndex` or `SimilarityMetric` and `@register` themselves.
3. **Edit `plugin.yaml`**: name, version, configuration and secret *names*. See [PLUGIN_MANIFEST.md](../docs/PLUGIN_MANIFEST.md).
4. **Verify locally**:
   ```bash
   go build -o llm-cache-plugin ./cmd/llm-cache-plugin      # from a checkout of LLM_CACHE
   ./llm-cache-plugin manifest plugin.yaml                  # manifest only
   ./llm-cache-plugin verify .                              # builds it, starts it isolated, runs the contract suite (needs Docker)
   ./llm-cache-plugin verify . --endpoint http://127.0.0.1:8080   # or test a plugin you already run (no Docker)
   ```
5. **Install it**: web UI -> *Plugins* -> *Add plugin* -> *GitHub repository* -> **Verify & Activate**.

## What the platform does with your plugin

Go plugins run as your own container, started read-only, non-root, without capabilities, with CPU/memory/process
limits and a private network. Python plugins run in a **runner image built from the platform's own service plus your
package** - never injected into a running service. Secrets you declare arrive as environment variables at start; the
platform also sets `PORT` and `PLUGIN_AUTH_TOKEN` (your service should require `Authorization: Bearer <that>` on every
route except `/health`; every Go example does). See [PLUGIN_SECURITY.md](../docs/PLUGIN_SECURITY.md).

## Rules of thumb

* Keep `/health` cheap and unauthenticated. Return client errors as `4xx` and never hang on bad input.
* Honour request cancellation and keep responses under the size limits.
* Never log secrets, prompts or provider error bodies.
* Do not write outside `/tmp`; the root filesystem is read-only. Persistent plugins must use an external database.
* Vectors are unit length, distances are "lower is more similar", a hit is `distance <= threshold`.
