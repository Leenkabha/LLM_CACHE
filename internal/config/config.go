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

	// Backend selectors choose which adapter implements each pluggable seam.
	// Each maps to a factory in the matching internal package, so a new
	// implementation is opt-in via configuration without any code changes to
	// the orchestrator. Defaults reproduce the demo stack.
	EmbeddingBackend   string // embedder adapter: "http"
	VectorStoreBackend string // vector-store adapter: "http"
	PersistenceBackend string // persistence adapter: "redis" or "memory"
	QueueBackend       string // cache-update queue adapter: "redis"

	LLMMode     string // registered LLM backend, e.g. "stub", "openai", "gemini", "example-http"
	OpenAIKey   string
	OpenAIModel string
	GeminiKey   string
	GeminiModel string
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
	threshold, _ := strconv.ParseFloat(getenv("SIMILARITY_THRESHOLD", "0.25"), 64)
	capacity, _ := strconv.Atoi(getenv("CACHE_CAPACITY", "1000"))

	return Config{
		TopK:           topK,
		Addr:           getenv("ORCH_ADDR", ":8080"),
		EmbeddingURL:   getenv("EMBEDDING_URL", "http://localhost:8001"),
		VectorStoreURL: getenv("VECTORSTORE_URL", "http://localhost:8002"),
		RedisAddr:      getenv("REDIS_ADDR", "localhost:6379"),
		Threshold:      threshold,
		Capacity:       capacity,
		Policy:         getenv("CACHE_POLICY", "lru"),
		LogFile:        getenv("LOG_FILE", "logs/orchestrator.log"),

		EmbeddingBackend:   getenv("EMBEDDING_BACKEND", "http"),
		VectorStoreBackend: getenv("VECTORSTORE_BACKEND", "http"),
		PersistenceBackend: getenv("PERSISTENCE_BACKEND", "redis"),
		QueueBackend:       getenv("QUEUE_BACKEND", "redis"),

		LLMMode:     getenv("LLM_MODE", "stub"),
		OpenAIKey:   getenv("OPENAI_API_KEY", ""),
		OpenAIModel: getenv("OPENAI_MODEL", "gpt-4o-mini"),
		GeminiKey:   getenv("GEMINI_API_KEY", ""),
		GeminiModel: getenv("GEMINI_MODEL", "gemini-flash-latest"),
	}, nil
}
