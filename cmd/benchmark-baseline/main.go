// baseline invokes the project's real LLM adapter without embedding or caching.
package main

import (
	"encoding/json"
	"github.com/leenkabha/llm_cache/internal/config"
	"github.com/leenkabha/llm_cache/internal/llm"
	"log"
	"net/http"
	"os"
	"sync/atomic"
	"time"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatal("invalid configuration")
	}
	if cfg.LLMFallbackMode != "" {
		log.Fatal("benchmark requires LLM fallback to be disabled")
	}
	if cfg.LLMMode == "stub" && os.Getenv("BENCH_ALLOW_SIMULATION") != "1" {
		log.Fatal("stub disabled: real-provider evidence required")
	}
	backend, err := llm.New(cfg)
	if err != nil {
		log.Fatal("LLM initialization failed")
	}
	var calls, failed atomic.Int64
	mux := http.NewServeMux()
	respond := func(w http.ResponseWriter, value any) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(value)
	}
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		model := cfg.OpenAIModel
		if cfg.LLMMode == "gemini" {
			model = cfg.GeminiModel
		}
		if cfg.LLMMode == "example-http" {
			model = os.Getenv("EXAMPLE_LLM_MODEL")
		}
		respond(w, map[string]any{"status": "ok", "mode": "no-cache", "provider": cfg.LLMMode, "model": model, "fallback": false, "completion_calls": calls.Load(), "failed_calls": failed.Load()})
	})
	mux.HandleFunc("POST /query", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Prompt string `json:"prompt"`
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in) != nil || in.Prompt == "" {
			http.Error(w, "invalid prompt", 400)
			return
		}
		start := time.Now()
		calls.Add(1)
		reply, err := backend.Complete(r.Context(), in.Prompt)
		if err != nil {
			failed.Add(1)
			http.Error(w, "LLM call failed; verify provider configuration and quota", 502)
			return
		}
		respond(w, map[string]any{"reply": reply, "source": "llm", "cache_hit": false, "latency_ms": time.Since(start).Milliseconds()})
	})
	log.Print("No-cache baseline listening on :8080 (same project LLM adapter)")
	log.Fatal(http.ListenAndServe(":8080", mux))
}
