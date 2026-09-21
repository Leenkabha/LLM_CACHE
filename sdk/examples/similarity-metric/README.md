# similarity-metric example: `sdk-angular-metric`

Python SimilarityMetric example (angular distance). It implements the **similarity-metric** contract: it registers a SimilarityMetric, and in the LLM
Cache it serves as the similarity metric (runs inside a vector runner; no per-score network call).

## Files

| File | Purpose |
| --- | --- |
| `plugin.yaml` | strict, versioned manifest (see [PLUGIN_MANIFEST.md](../../../docs/PLUGIN_MANIFEST.md)) |
| ``sdk_angular_metric/__init__.py` | the minimal working implementation |
| ``requirements.txt` | extra Python dependencies installed into the runner image |
| `config.example.json` | an example install request body |
| `expected-responses.md` | what the contract tests expect to see |

## Try it

```bash
llm-cache-plugin verify .            # builds the runner image locally, starts it isolated, runs the contract suite
```

## Make it yours

1. Copy this directory into a new Git repository and replace the implementation.
2. Keep `plugin.yaml` valid: change `metadata.name` and `metadata.version`, keep `spec.type: similarity-metric`.
3. Run `llm-cache-plugin verify .` until every check passes.
4. Push to GitHub, open the LLM Cache web UI, **Plugins -> Add plugin -> GitHub repository**,
   choose type `similarity-metric`, paste the repository URL and click **Verify & Activate**.

## Request / response

```
POST /v1/metric/distance
{"scores":[-1,0,0.5,1]}

200
{"distances":[1.0,0.5,0.333,0.0]}
```

## Activation notes

Metric scales differ, so activation requires you to confirm a similarity threshold. Only FAISS inner-product and L2 scores are supported.

The runner image is built from the platform's own service plus your package; you never edit or rebuild the service yourself. Your package imports the platform's registry (`from app... import register`), which exists inside the runner.

Secrets are declared by name in `plugin.yaml` and entered separately in the UI; they reach your process
only as environment variables at start. Never put a secret value in the repository.
