package orchestrator

import (
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"net/http"
	"strings"
)

// webFS embeds the demo UI so it ships inside the orchestrator binary --
// no extra files to copy in the Docker build, no separate static-file
// service, no CORS: the UI is served from the same origin as the API it
// calls.
//
//go:embed web/index.html web/plugins.html
var webFS embed.FS

func (s *Service) handleUI(w http.ResponseWriter, r *http.Request) {
	data, err := webFS.ReadFile("web/index.html")
	if err != nil {
		http.Error(w, "ui not available", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(data)
}

// handlePluginsUI serves the plugin-management page. The page itself is static;
// every API call it makes is authenticated with the administrator's token.
func (s *Service) handlePluginsUI(w http.ResponseWriter, r *http.Request) {
	data, err := webFS.ReadFile("web/plugins.html")
	if err != nil {
		http.Error(w, "ui not available", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// The page has one inline script. Allow exactly that script by hash instead of
	// 'unsafe-inline', so injected script (there is no HTML insertion of API data
	// anyway) would still not run.
	scriptSrc := "'none'"
	if start := strings.Index(string(data), "<script>"); start >= 0 {
		if end := strings.Index(string(data)[start:], "</script>"); end > 0 {
			sum := sha256.Sum256(data[start+len("<script>") : start+end])
			scriptSrc = "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
		}
	}
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src "+scriptSrc+"; style-src 'unsafe-inline'; connect-src 'self'; img-src 'self' data:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
}
