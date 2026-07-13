// Package config loads orchestrator configuration from environment variables.
package config

import (
	"os"
	"strconv"
)

type Config struct {
	Addr           string  // address the orchestrator HTTP server listens on
	EmbeddingURL   string  // base URL of the embedding service
	VectorStoreURL string  // base URL of the vector-store service
	RedisAddr      string  // host:port of Redis persistence
	Threshold      float64 // max distance for a cache hit
	Capacity       int     // max number of cached entries
	Policy         string  // eviction policy: "lru" or "lfu"

	LLMMode     string // "stub" or "openai"
	OpenAIKey   string
	OpenAIModel string
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// Load reads configuration from the environment, applying sensible defaults
// so the orchestrator can boot for local development without an .env file.
func Load() Config {
	threshold, _ := strconv.ParseFloat(getenv("SIMILARITY_THRESHOLD", "0.25"), 64)
	capacity, _ := strconv.Atoi(getenv("CACHE_CAPACITY", "1000"))

	return Config{
		Addr:           getenv("ORCH_ADDR", ":8080"),
		EmbeddingURL:   getenv("EMBEDDING_URL", "http://localhost:8001"),
		VectorStoreURL: getenv("VECTORSTORE_URL", "http://localhost:8002"),
		RedisAddr:      getenv("REDIS_ADDR", "localhost:6379"),
		Threshold:      threshold,
		Capacity:       capacity,
		Policy:         getenv("CACHE_POLICY", "lru"),
		LLMMode:        getenv("LLM_MODE", "stub"),
		OpenAIKey:      getenv("OPENAI_API_KEY", ""),
		OpenAIModel:    getenv("OPENAI_MODEL", "gpt-4o-mini"),
	}
}
