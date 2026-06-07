// Command orchestrator is the central cache coordinator service.
package main

import (
	"log"
	"net/http"

	"github.com/leenkabha/llm_cache/internal/config"
	"github.com/leenkabha/llm_cache/internal/orchestrator"
)

func main() {
	cfg := config.Load()
	svc := orchestrator.New(cfg)

	log.Printf("orchestrator listening on %s (llm_mode=%s, threshold=%.3f, policy=%s)",
		cfg.Addr, cfg.LLMMode, cfg.Threshold, cfg.Policy)
	if err := http.ListenAndServe(cfg.Addr, svc.Routes()); err != nil {
		log.Fatalf("server stopped: %v", err)
	}
}
