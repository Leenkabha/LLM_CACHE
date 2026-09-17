// Command orchestrator is the central cache coordinator service.
package main

import (
	"log"
	"net/http"

	"github.com/leenkabha/llm_cache/internal/config"
	applog "github.com/leenkabha/llm_cache/internal/logging"
	"github.com/leenkabha/llm_cache/internal/orchestrator"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	closeLog, err := applog.Setup(cfg.LogFile)
	if err != nil {
		log.Fatalf("setup logging: %v", err)
	}
	defer func() {
		if err := closeLog(); err != nil {
			log.Printf("close log file: %v", err)
		}
	}()

	svc, err := orchestrator.New(cfg)
	if err != nil {
		log.Fatalf("create orchestrator: %v", err)
	}

	log.Printf("orchestrator listening on %s (llm_mode=%s, threshold=%.3f, policy=%s, redis=%s, log_file=%s)",
		cfg.Addr, cfg.LLMMode, cfg.Threshold, cfg.Policy, cfg.RedisAddr, cfg.LogFile)
	if err := http.ListenAndServe(cfg.Addr, svc.Routes()); err != nil {
		log.Fatalf("server stopped: %v", err)
	}
}
