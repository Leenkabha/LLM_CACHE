# vector-index example: `sdk-numpy-index`

Python VectorIndex example. It implements the **vector-index** contract: it registers a VectorIndex, and in the LLM
Cache it serves as the vector index (runs in a runner built from the platform's vector-store service).

## Files

| File | Purpose |
| --- | --- |
| `plugin.yaml` | strict, versioned manifest (see [PLUGIN_MANIFEST.md](../../../docs/PLUGIN_MANIFEST.md)) |
| ``sdk_numpy_index/__init__.py` | the minimal working implementation |
| ``requirements.txt` | extra Python dependencies installed into the runner image |
| `config.example.json` | an example install request body |
| `expected-responses.md` | what the contract tests expect to see |

## Try it

```bash
llm-cache-plugin verify .            # builds the runner image locally, starts it isolated, runs the contract suite
```

## Make it yours

1. Copy this directory into a new Git repository and replace the implementation.
2. Keep `plugin.yaml` valid: change `metadata.name` and `metadata.version`, keep `spec.type: vector-index`.
3. Run `llm-cache-plugin verify .` until every check passes.
4. Push to GitHub, open the LLM Cache web UI, **Plugins -> Add plugin -> GitHub repository**,
   choose type `vector-index`, paste the repository URL and click **Verify & Activate**.

## Request / response

```
POST /v1/search
{"vector":[...],"top_k":3,"threshold":0.25}

200
{"matches":[{"id":"a1","distance":0.02}],"hit":true,"id":"a1","distance":0.02}
```

## Activation notes

The full vector-store contract suite runs against your index before activation, and the candidate is rebuilt from persistence.

The runner image is built from the platform's own service plus your package; you never edit or rebuild the service yourself. Your package imports the platform's registry (`from app... import register`), which exists inside the runner.

Secrets are declared by name in `plugin.yaml` and entered separately in the UI; they reach your process
only as environment variables at start. Never put a secret value in the repository.
