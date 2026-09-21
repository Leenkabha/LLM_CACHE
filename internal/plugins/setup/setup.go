// Package setup wires the plugin platform into the orchestrator at startup.
//
// With ENABLE_PLUGIN_INSTALLATION unset (the default) Prepare returns the
// dependencies untouched and nothing about the orchestrator changes. When it is
// enabled, the six seams are wrapped in swappable adapters, the persisted plugin
// selection is restored BEFORE the orchestrator rebuilds its state, and the
// admin API is mounted.
package setup

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/leenkabha/llm_cache/internal/config"
	"github.com/leenkabha/llm_cache/internal/orchestrator"
	"github.com/leenkabha/llm_cache/internal/plugins"
	"github.com/leenkabha/llm_cache/internal/plugins/admin"
	"github.com/leenkabha/llm_cache/internal/plugins/ctlapi"
	"github.com/leenkabha/llm_cache/internal/plugins/manager"
	"github.com/leenkabha/llm_cache/internal/plugins/manifest"
	"github.com/leenkabha/llm_cache/internal/plugins/registry"
	"github.com/leenkabha/llm_cache/internal/plugins/secrets"
)

// Platform is the running plugin platform (nil-safe when disabled).
type Platform struct {
	Deps    orchestrator.Dependencies
	Manager *manager.Manager
	admin   http.Handler
}

// ManagerConfig converts environment configuration into the manager's, failing
// fast on anything invalid.
func ManagerConfig(cfg config.Config) (manager.Config, error) {
	mc := manager.Config{
		Enabled: cfg.EnablePluginInstallation, AllowInsecureEndpoints: cfg.AllowInsecurePluginEndpoints,
		ControllerURL: cfg.PluginControllerURL, ControllerToken: cfg.PluginControllerToken,
	}
	dur := func(name, v string, dst *time.Duration) error {
		if v == "" {
			return nil
		}
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return fmt.Errorf("%s must be a positive duration such as 60s", name)
		}
		*dst = d
		return nil
	}
	if err := dur("PLUGIN_BUILD_TIMEOUT", cfg.PluginBuildTimeout, &mc.BuildTimeout); err != nil {
		return mc, err
	}
	if err := dur("PLUGIN_HEALTH_TIMEOUT", cfg.PluginHealthTimeout, &mc.HealthTimeout); err != nil {
		return mc, err
	}
	if err := dur("PLUGIN_ROLLBACK_WINDOW", cfg.PluginRollbackWindow, &mc.RollbackWindow); err != nil {
		return mc, err
	}
	if cfg.PluginCPULimit != "" {
		c, err := manifest.ParseCPU(cfg.PluginCPULimit)
		if err != nil {
			return mc, fmt.Errorf("PLUGIN_CPU_LIMIT: %w", err)
		}
		mc.CPULimit = c
	}
	if cfg.PluginMemoryLimit != "" {
		b, err := manifest.ParseBytes(cfg.PluginMemoryLimit)
		if err != nil {
			return mc, fmt.Errorf("PLUGIN_MEMORY_LIMIT: %w", err)
		}
		mc.MemoryLimit = b
	}
	if mc.ControllerURL != "" && mc.ControllerToken == "" {
		return mc, errors.New("PLUGIN_CONTROLLER_URL is set but PLUGIN_CONTROLLER_TOKEN is empty; the controller must be authenticated")
	}
	return mc, nil
}

// Prepare wires the platform. When disabled it returns deps unchanged.
func Prepare(cfg config.Config, deps orchestrator.Dependencies) (*Platform, error) {
	p := &Platform{Deps: deps}
	if !cfg.EnablePluginInstallation {
		return p, nil
	}
	if cfg.AdminToken == "" {
		return nil, errors.New("ENABLE_PLUGIN_INSTALLATION=true requires a non-empty ADMIN_TOKEN: plugin management is never exposed unauthenticated")
	}
	box, err := secrets.NewBox(cfg.PluginSecretKey)
	if err != nil {
		return nil, fmt.Errorf("ENABLE_PLUGIN_INSTALLATION=true requires a valid key: %w", err)
	}
	mc, err := ManagerConfig(cfg)
	if err != nil {
		return nil, err
	}
	var reg registry.Store
	switch strings.ToLower(cfg.PluginRegistryBackend) {
	case "memory":
		log.Printf("plugin registry: in memory (plugin records are lost on restart)")
		reg = registry.NewMemory()
	case "", "redis":
		if reg, err = registry.NewRedis(cfg.RedisAddr); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("PLUGIN_REGISTRY_BACKEND must be redis or memory")
	}
	var ctl manager.Controller
	if mc.ControllerURL != "" {
		ctl = ctlapi.NewClient(mc.ControllerURL, mc.ControllerToken)
	}

	slots := manager.NewSlots(deps.LLM, deps.Embedder, deps.VectorStore, deps.Store, deps.Queue, deps.Policy)
	m := manager.New(mc, reg, box, slots, ctl)
	m.SetBuiltinNames(map[plugins.Type]string{
		plugins.TypeLLM: orDefault(cfg.LLMMode, "stub"), plugins.TypeEmbedder: orDefault(cfg.EmbeddingBackend, "http"),
		plugins.TypeVectorStore: orDefault(cfg.VectorStoreBackend, "http"), plugins.TypePersistence: orDefault(cfg.PersistenceBackend, "redis"),
		plugins.TypeQueue: orDefault(cfg.QueueBackend, "redis"), plugins.TypePolicy: orDefault(cfg.Policy, "lru"),
		plugins.TypeEmbeddingModel: "sentence-transformers", plugins.TypeVectorIndex: "faiss", plugins.TypeSimilarityMetric: "cosine",
	})

	// Restore the persisted selection now, before the orchestrator rebuilds the
	// vector store and eviction policy from persistence.
	rctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := m.Restore(rctx); err != nil {
		return nil, err
	}

	p.Manager = m
	p.admin = admin.New(m, cfg.AdminToken, admin.WithTrustProxy(cfg.TrustProxy))
	p.Deps = orchestrator.Dependencies{
		Embedder: slots.Embedder, VectorStore: slots.VectorStore, LLM: slots.LLM,
		Store: slots.Store, Queue: slots.Queue, Policy: slots.Policy,
	}
	log.Printf("plugin platform enabled (controller_configured=%v, insecure_endpoints=%v)", ctl != nil, mc.AllowInsecureEndpoints)
	return p, nil
}

// Attach connects the running orchestrator to the platform.
func (p *Platform) Attach(svc *orchestrator.Service) {
	if p.Manager == nil {
		return
	}
	if t := p.Manager.RestoredThreshold(); t != nil {
		svc.SetThreshold(*t)
	}
	p.Manager.SetHost(svc)
	svc.SetAdminHandler(p.admin)
	p.Manager.StartMonitor(context.Background(), 30*time.Second)
}

func orDefault(s, d string) string {
	if strings.TrimSpace(s) == "" {
		return d
	}
	return s
}
