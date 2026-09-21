// Package config loads orchestrator configuration from environment variables.
package config

import (
	"fmt"
	"os"
	"strconv"
)

type Config struct {
	TopK           int     // maximum accepted cached replies per query
	Addr           string  // address the orchestrator HTTP server listens on
	EmbeddingURL   string  // base URL of the embedding service
	VectorStoreURL string  // base URL of the vector-store service
	RedisAddr      string  // host:port of Redis persistence
	Threshold      float64 // max distance for a cache hit
	Capacity       int     // max number of cached entries
	Policy         string  // registered eviction policy, e.g. "lru", "lfu", "fifo"
	LogFile        string  // optional path for persistent orchestrator logs

	// Public-deployment protections; all off by default for local use.
	AdminToken      string // when set, POST /flush and /policy need "Authorization: Bearer <token>"
	RateLimitPerMin int    // when > 0, max POST /query requests per client IP per minute
	TrustProxy      bool   // use X-Forwarded-For for the client IP (only behind a reverse proxy)

	// Backend selectors choose which adapter implements each pluggable seam.
	// Each maps to a factory in the matching internal package, so a new
	// implementation is opt-in via configuration without any code changes to
	// the orchestrator. Defaults reproduce the demo stack.
	EmbeddingBackend   string // embedder adapter: "http"
	VectorStoreBackend string // vector-store adapter: "http"
	PersistenceBackend string // persistence adapter: "redis" or "memory"
	QueueBackend       string // cache-update queue adapter: "redis"

	LLMMode         string // registered LLM backend, e.g. "stub", "openai", "gemini", "example-http"
	LLMFallbackMode string // optional: registered LLM backend to fall back to if LLMMode fails; empty disables fallback
	OpenAIKey       string
	OpenAIModel     string
	GeminiKey       string
	GeminiModel     string

	// Optional Gemini budgets used only to log how much is left (Gemini does not
	// report remaining quota). 0 means unknown/unlimited.
	GeminiDailyRequestLimit int // e.g. 20 on the free tier
	GeminiDailyTokenBudget  int // your own per-day token allowance

	// Plugin platform. Everything here is off by default; with
	// EnablePluginInstallation false the orchestrator behaves exactly as before
	// and the /admin/plugins API does not exist. Parsing and validation of the
	// values happens in internal/plugins/setup so this package stays dependency-free.
	EnablePluginInstallation     bool   // ENABLE_PLUGIN_INSTALLATION
	PluginSecretKey              string // PLUGIN_SECRET_KEY: base64 of 32 random bytes; encrypts plugin secrets
	PluginControllerURL          string // PLUGIN_CONTROLLER_URL: internal controller; empty disables image/repository installs
	PluginControllerToken        string // PLUGIN_CONTROLLER_TOKEN: shared secret with the controller
	AllowInsecurePluginEndpoints bool   // ALLOW_INSECURE_PLUGIN_ENDPOINTS: http, loopback, private addresses (development only)
	PluginRegistryBackend        string // PLUGIN_REGISTRY_BACKEND: redis (default) or memory
	PluginBuildTimeout           string // PLUGIN_BUILD_TIMEOUT, e.g. 10m
	PluginHealthTimeout          string // PLUGIN_HEALTH_TIMEOUT, e.g. 60s
	PluginCPULimit               string // PLUGIN_CPU_LIMIT: default and ceiling per plugin, e.g. 1 or 500m
	PluginMemoryLimit            string // PLUGIN_MEMORY_LIMIT: default and ceiling per plugin, e.g. 512Mi
	PluginRollbackWindow         string // PLUGIN_ROLLBACK_WINDOW, e.g. 10m
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// Load reads configuration from the environment, applying sensible defaults
// so the orchestrator can boot for local development without an .env file.
func Load() (Config, error) {
	topK, err := strconv.Atoi(getenv("CACHE_TOP_K", "1"))
	if err != nil || topK < 1 {
		return Config{}, fmt.Errorf("CACHE_TOP_K must be a positive integer")
	}
	threshold, err := strconv.ParseFloat(getenv("SIMILARITY_THRESHOLD", "0.25"), 64)
	if err != nil {
		return Config{}, fmt.Errorf("SIMILARITY_THRESHOLD must be a number")
	}
	capacity, err := strconv.Atoi(getenv("CACHE_CAPACITY", "1000"))
	if err != nil {
		return Config{}, fmt.Errorf("CACHE_CAPACITY must be an integer")
	}

	rateLimit, err := strconv.Atoi(getenv("RATE_LIMIT_PER_MIN", "0"))
	if err != nil || rateLimit < 0 {
		return Config{}, fmt.Errorf("RATE_LIMIT_PER_MIN must be a non-negative integer")
	}
	trustProxy, err := strconv.ParseBool(getenv("TRUST_PROXY", "false"))
	if err != nil {
		return Config{}, fmt.Errorf("TRUST_PROXY must be true or false")
	}

	geminiReqLimit, err := strconv.Atoi(getenv("GEMINI_DAILY_REQUEST_LIMIT", "0"))
	if err != nil || geminiReqLimit < 0 {
		return Config{}, fmt.Errorf("GEMINI_DAILY_REQUEST_LIMIT must be a non-negative integer")
	}
	geminiTokenBudget, err := strconv.Atoi(getenv("GEMINI_DAILY_TOKEN_BUDGET", "0"))
	if err != nil || geminiTokenBudget < 0 {
		return Config{}, fmt.Errorf("GEMINI_DAILY_TOKEN_BUDGET must be a non-negative integer")
	}

	enablePlugins, err := strconv.ParseBool(getenv("ENABLE_PLUGIN_INSTALLATION", "false"))
	if err != nil {
		return Config{}, fmt.Errorf("ENABLE_PLUGIN_INSTALLATION must be true or false")
	}
	insecureEndpoints, err := strconv.ParseBool(getenv("ALLOW_INSECURE_PLUGIN_ENDPOINTS", "false"))
	if err != nil {
		return Config{}, fmt.Errorf("ALLOW_INSECURE_PLUGIN_ENDPOINTS must be true or false")
	}

	return Config{
		EnablePluginInstallation:     enablePlugins,
		PluginSecretKey:              getenv("PLUGIN_SECRET_KEY", ""),
		PluginControllerURL:          getenv("PLUGIN_CONTROLLER_URL", ""),
		PluginControllerToken:        getenv("PLUGIN_CONTROLLER_TOKEN", ""),
		AllowInsecurePluginEndpoints: insecureEndpoints,
		PluginRegistryBackend:        getenv("PLUGIN_REGISTRY_BACKEND", "redis"),
		PluginBuildTimeout:           getenv("PLUGIN_BUILD_TIMEOUT", ""),
		PluginHealthTimeout:          getenv("PLUGIN_HEALTH_TIMEOUT", ""),
		PluginCPULimit:               getenv("PLUGIN_CPU_LIMIT", ""),
		PluginMemoryLimit:            getenv("PLUGIN_MEMORY_LIMIT", ""),
		PluginRollbackWindow:         getenv("PLUGIN_ROLLBACK_WINDOW", ""),
		AdminToken:                   getenv("ADMIN_TOKEN", ""),
		RateLimitPerMin:              rateLimit,
		TrustProxy:                   trustProxy,
		TopK:                         topK,
		Addr:                         getenv("ORCH_ADDR", ":8080"),
		EmbeddingURL:                 getenv("EMBEDDING_URL", "http://localhost:8001"),
		VectorStoreURL:               getenv("VECTORSTORE_URL", "http://localhost:8002"),
		RedisAddr:                    getenv("REDIS_ADDR", "localhost:6379"),
		Threshold:                    threshold,
		Capacity:                     capacity,
		Policy:                       getenv("CACHE_POLICY", "lru"),
		LogFile:                      getenv("LOG_FILE", "logs/orchestrator.log"),

		EmbeddingBackend:   getenv("EMBEDDING_BACKEND", "http"),
		VectorStoreBackend: getenv("VECTORSTORE_BACKEND", "http"),
		PersistenceBackend: getenv("PERSISTENCE_BACKEND", "redis"),
		QueueBackend:       getenv("QUEUE_BACKEND", "redis"),

		LLMMode:         getenv("LLM_MODE", "stub"),
		LLMFallbackMode: getenv("LLM_FALLBACK_MODE", ""),
		OpenAIKey:       getenv("OPENAI_API_KEY", ""),
		OpenAIModel:     getenv("OPENAI_MODEL", "gpt-4o-mini"),
		GeminiKey:       getenv("GEMINI_API_KEY", ""),
		GeminiModel:     getenv("GEMINI_MODEL", "gemini-flash-latest"),

		GeminiDailyRequestLimit: geminiReqLimit,
		GeminiDailyTokenBudget:  geminiTokenBudget,
	}, nil
}
