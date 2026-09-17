package orchestrator

import (
	"embed"
	"net/http"
)

// webFS embeds the demo UI so it ships inside the orchestrator binary --
// no extra files to copy in the Docker build, no separate static-file
// service, no CORS: the UI is served from the same origin as the API it
// calls.
//
//go:embed web/index.html
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
