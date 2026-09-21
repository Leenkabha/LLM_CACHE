# Expected responses (embedding-model)

The contract suite (`internal/plugins/contract`, run by `llm-cache-plugin verify`) drives the plugin
through the versioned protocol and checks, among other things:

```
POST /v1/embed
{"text":"hello"}

200
{"vector":[...],"dim":384,"model":"sdk-hash-d384"}
```

See [PLUGIN_CONTRACTS.md](../../../docs/PLUGIN_CONTRACTS.md) for the full protocol and the list of checks
for this type. A failing check prints its name and the reason.
