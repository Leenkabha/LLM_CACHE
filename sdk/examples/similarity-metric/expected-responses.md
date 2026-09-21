# Expected responses (similarity-metric)

The contract suite (`internal/plugins/contract`, run by `llm-cache-plugin verify`) drives the plugin
through the versioned protocol and checks, among other things:

```
POST /v1/metric/distance
{"scores":[-1,0,0.5,1]}

200
{"distances":[1.0,0.5,0.333,0.0]}
```

See [PLUGIN_CONTRACTS.md](../../../docs/PLUGIN_CONTRACTS.md) for the full protocol and the list of checks
for this type. A failing check prints its name and the reason.
