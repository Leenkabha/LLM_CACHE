# llm example: `sdk-echo-llm`

Deterministic echo LLM for testing the LLM Cache plugin platform. It implements the **llm** contract: it answers a prompt with a reply, and in the LLM
Cache it serves as the LLM.

## Files

| File | Purpose |
| --- | --- |
| `plugin.yaml` | strict, versioned manifest (see [PLUGIN_MANIFEST.md](../../../docs/PLUGIN_MANIFEST.md)) |
| ``main.go` | the minimal working implementation |
| ``Dockerfile` | builds a small non-root image |
| `config.example.json` | an example install request body |
| `expected-responses.md` | what the contract tests expect to see |

## Try it

```bash
llm-cache-plugin verify .            # builds the image, starts it isolated, runs the contract suite
llm-cache-plugin verify . --endpoint http://127.0.0.1:8080   # or test a plugin you already started
```

To run it by hand: `go run .` (listens on `:8080`, override with `PORT`).

## Make it yours

1. Copy this directory into a new Git repository and replace the implementation.
2. Keep `plugin.yaml` valid: change `metadata.name` and `metadata.version`, keep `spec.type: llm`.
3. Run `llm-cache-plugin verify .` until every check passes.
4. Push to GitHub, open the LLM Cache web UI, **Plugins -> Add plugin -> GitHub repository**,
   choose type `llm`, paste the repository URL and click **Verify & Activate**.

## Request / response

```
POST /v1/complete
{"model":"demo","prompt":"hello"}

200
{"reply":"[sdk-llm demo ok] echo: hello"}
```

## Activation notes

Cached replies stay valid across an LLM swap; activation is a plain hot swap.

The image is started with a read-only root filesystem, a small `/tmp`, no privileges, CPU/memory limits and no network egress unless you ask for it in `plugin.yaml`.

Secrets are declared by name in `plugin.yaml` and entered separately in the UI; they reach your process
only as environment variables at start. Never put a secret value in the repository.
