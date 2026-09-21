# Manifest templates

One valid `plugin.yaml` per plugin type: `<type>.plugin.yaml`. They are the manifests of the matching
`../examples/<type>/` plugin, so they are always valid (a test parses every one). Full reference:
[docs/PLUGIN_MANIFEST.md](../../docs/PLUGIN_MANIFEST.md).

`plugin.schema.json` is a JSON Schema for editor completion. It describes the shape only - the authoritative,
stricter validation (dangerous-field rejection, secret detection, path and resource checks) is
`llm-cache-plugin manifest plugin.yaml`, which runs the same code as the platform.

Validate your manifest before pushing:

```bash
llm-cache-plugin manifest plugin.yaml
```
