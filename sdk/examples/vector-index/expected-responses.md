# Expected responses (vector-index)

The contract suite (`internal/plugins/contract`, run by `llm-cache-plugin verify`) drives the plugin
through the versioned protocol and checks, among other things:

```
POST /v1/search
{"vector":[...],"top_k":3,"threshold":0.25}

200
{"matches":[{"id":"a1","distance":0.02}],"hit":true,"id":"a1","distance":0.02}
```

See [PLUGIN_CONTRACTS.md](../../../docs/PLUGIN_CONTRACTS.md) for the full protocol and the list of checks
for this type. A failing check prints its name and the reason.
