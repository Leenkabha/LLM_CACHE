// Command orchestrator is the central cache coordinator service.
package main

import (
	"log"
	"net/http"
	"time"

	"github.com/leenkabha/llm_cache/internal/config"
	applog "github.com/leenkabha/llm_cache/internal/logging"
	"github.com/leenkabha/llm_cache/internal/orchestrator"
	"github.com/leenkabha/llm_cache/internal/plugins/setup"
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

	// A whole-stack restart starts every container at once; wait (up to a minute) for
	// Redis and the other services to accept connections instead of exiting.
	var deps orchestrator.Dependencies
	if err := orchestrator.RetryTransient("dependencies", time.Minute, 2*time.Second, func() (err error) {
		deps, err = orchestrator.BuildDependencies(cfg)
		return err
	}); err != nil {
		log.Fatalf("create orchestrator: %v", err)
	}
	// The plugin platform is opt-in (ENABLE_PLUGIN_INSTALLATION); when off, deps
	// pass through untouched. When on, it restores the persisted plugin selection
	// before the orchestrator rebuilds its state from persistence.
	var platform *setup.Platform
	if err := orchestrator.RetryTransient("plugin platform", time.Minute, 2*time.Second, func() (err error) {
		platform, err = setup.Prepare(cfg, deps)
		return err
	}); err != nil {
		log.Fatalf("plugin platform: %v", err)
	}
	svc, err := orchestrator.NewWithDependencies(cfg, platform.Deps)
	if err != nil {
		log.Fatalf("create orchestrator: %v", err)
	}
	platform.Attach(svc)

	log.Printf("orchestrator listening on %s (llm_mode=%s, threshold=%.3f, policy=%s, redis=%s, log_file=%s)",
		cfg.Addr, cfg.LLMMode, cfg.Threshold, cfg.Policy, cfg.RedisAddr, cfg.LogFile)
	if err := http.ListenAndServe(cfg.Addr, svc.Routes()); err != nil {
		log.Fatalf("server stopped: %v", err)
	}
}
