// Command llm-server is a local, deterministic example provider. It does not
// call an AI model. Pair it with LLM_MODE=example-http to test custom wiring.
package main

import (
	"encoding/json"
	"log"
	"net/http"
	"time"
)

func routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})
	mux.HandleFunc("POST /complete", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Model  string `json:"model"`
			Prompt string `json:"prompt"`
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Model == "" || in.Prompt == "" {
			http.Error(w, "model and prompt required", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"reply": "[example " + in.Model + "] " + in.Prompt})
	})
	return mux
}

func main() {
	server := &http.Server{Addr: ":8090", Handler: routes(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second}
	log.Print("example LLM server listening on :8090 (deterministic demo, no AI model)")
	log.Fatal(server.ListenAndServe())
}
