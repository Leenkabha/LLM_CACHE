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
	svc, err := orchestrator.New(cfg)
	if err != nil {
		log.Fatalf("create orchestrator: %v", err)
	}

	log.Printf("orchestrator listening on %s (llm_mode=%s, threshold=%.3f, policy=%s, redis=%s)",
		cfg.Addr, cfg.LLMMode, cfg.Threshold, cfg.Policy, cfg.RedisAddr)
	if err := http.ListenAndServe(cfg.Addr, svc.Routes()); err != nil {
		log.Fatalf("server stopped: %v", err)
	}
}
