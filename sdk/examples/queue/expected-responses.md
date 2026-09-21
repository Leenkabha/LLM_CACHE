# Expected responses (queue)

The contract suite (`internal/plugins/contract`, run by `llm-cache-plugin verify`) drives the plugin
through the versioned protocol and checks, among other things:

```
POST /v1/jobs/lease
{"max_jobs":1,"visibility_timeout_seconds":60,"wait_seconds":2}

200
{"jobs":[{"id":"j1","prompt":"p","reply":"r","vector":[...],"lease_token":"t","attempt":1}]}
```

See [PLUGIN_CONTRACTS.md](../../../docs/PLUGIN_CONTRACTS.md) for the full protocol and the list of checks
for this type. A failing check prints its name and the reason.
