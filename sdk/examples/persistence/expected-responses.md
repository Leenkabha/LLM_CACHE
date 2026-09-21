# Expected responses (persistence)

The contract suite (`internal/plugins/contract`, run by `llm-cache-plugin verify`) drives the plugin
through the versioned protocol and checks, among other things:

```
PUT /v1/entries/a1
{"id":"a1","prompt":"p","reply":"r","vector":[...],"created_at":"2026-01-01T00:00:00Z"}

204
```

See [PLUGIN_CONTRACTS.md](../../../docs/PLUGIN_CONTRACTS.md) for the full protocol and the list of checks
for this type. A failing check prints its name and the reason.
