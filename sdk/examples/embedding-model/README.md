# embedding-model example: `sdk-hash-model`

Python EmbeddingModel example. It implements the **embedding-model** contract: it registers an EmbeddingModel, and in the LLM
Cache it serves as the embedding model (runs in a runner built from the platform's embedding service).

## Files

| File | Purpose |
| --- | --- |
| `plugin.yaml` | strict, versioned manifest (see [PLUGIN_MANIFEST.md](../../../docs/PLUGIN_MANIFEST.md)) |
| ``sdk_hash_model/__init__.py` | the minimal working implementation |
| ``requirements.txt` | extra Python dependencies installed into the runner image |
| `config.example.json` | an example install request body |
| `expected-responses.md` | what the contract tests expect to see |

## Try it

```bash
llm-cache-plugin verify .            # builds the runner image locally, starts it isolated, runs the contract suite
```

## Make it yours

1. Copy this directory into a new Git repository and replace the implementation.
2. Keep `plugin.yaml` valid: change `metadata.name` and `metadata.version`, keep `spec.type: embedding-model`.
3. Run `llm-cache-plugin verify .` until every check passes.
4. Push to GitHub, open the LLM Cache web UI, **Plugins -> Add plugin -> GitHub repository**,
   choose type `embedding-model`, paste the repository URL and click **Verify & Activate**.

## Request / response

```
POST /v1/embed
{"text":"hello"}

200
{"vector":[...],"dim":384,"model":"sdk-hash-d384"}
```

## Activation notes

Built into a runner image: the platform copies your package into the embedding service's app/plugins/ and selects it. Model changes need the same flush/re-embed confirmation as an embedder.

The runner image is built from the platform's own service plus your package; you never edit or rebuild the service yourself. Your package imports the platform's registry (`from app... import register`), which exists inside the runner.

Secrets are declared by name in `plugin.yaml` and entered separately in the UI; they reach your process
only as environment variables at start. Never put a secret value in the repository.
