# Plugins UI smoke test (optional)

`plugins_ui_smoke.mjs` loads the real `internal/orchestrator/web/plugins.html` in jsdom with a fake admin API and checks
rendering of every state, the add-plugin flow, secrets handling (write-only, cleared, never in a URL), the confirmation
dialog, and that hostile API data is shown as text and never executed.

```bash
cd scripts/ui_smoke && npm install && node plugins_ui_smoke.mjs
```

Needs Node 18+. It is not part of `go test`; the Go tests in `internal/orchestrator/plugins_ui_test.go` cover the
page's static security properties and headers.
