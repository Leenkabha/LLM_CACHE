// Package embedder is the orchestrator's port to the embedding service.
//
// The orchestrator depends only on the Embedder interface, never on a concrete
// type. The demo ships one adapter -- an HTTP client that speaks the embedding
// service's REST contract (sentence-transformers today) -- but any other
// implementation (an in-process model, a different provider, a test double)
// can be plugged in without touching the orchestrator. Selecting an adapter is
// the factory's job (see New).
package embedder

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/leenkabha/llm_cache/internal/config"
	"github.com/leenkabha/llm_cache/internal/plugin"
)

// Embedder converts a text prompt into an embedding vector.
//
// This is the pluggable seam: the orchestrator holds an Embedder, so the
// concrete model/provider behind it is interchangeable.
type Embedder interface {
	// Embed converts a prompt into an embedding vector.
	Embed(ctx context.Context, text string) ([]float64, error)
	// Health reports whether the underlying embedding backend is reachable.
	Health(ctx context.Context) error
}

// Backend names the built-in embedder adapters selectable via configuration.
const (
	// BackendHTTP talks to a remote embedding service over REST (default).
	BackendHTTP = "http"
)

// registry holds every embedder adapter, keyed by the name used in
// EMBEDDING_BACKEND.
var registry = plugin.NewRegistry[Embedder]("embedder backend")

// Register makes an embedder adapter available under name.
//
// Call it from an init() in your adapter file. Because adapters live in package
// embedder, adding the file is enough -- no existing file changes. Then set
// EMBEDDING_BACKEND=<name> to select it.
func Register(name string, factory plugin.Factory[Embedder]) {
	registry.Register(name, factory)
}

// New builds the Embedder selected by cfg.EmbeddingBackend from the registry.
func New(cfg config.Config) (Embedder, error) {
	name := cfg.EmbeddingBackend
	if name == "" {
		name = BackendHTTP
	}
	return registry.Build(name, cfg)
}

// The built-in HTTP adapter registers itself.
func init() {
	Register(BackendHTTP, func(cfg config.Config) (Embedder, error) {
		return NewHTTP(cfg.EmbeddingURL), nil
	})
}

// httpEmbedder is the HTTP adapter for a remote embedding service. It depends
// only on the REST contract, so the underlying model is interchangeable.
type httpEmbedder struct {
	baseURL string
	http    *http.Client
}

// NewHTTP builds the HTTP embedder adapter directly. Prefer New for
// config-driven selection; this is exported for tests and explicit wiring.
func NewHTTP(baseURL string) Embedder {
	return &httpEmbedder{
		baseURL: baseURL,
		http:    &http.Client{Timeout: 10 * time.Second},
	}
}

type embedRequest struct {
	Text string `json:"text"`
}

type embedResponse struct {
	Vector []float64 `json:"vector"`
	Dim    int       `json:"dim"`
}

// Embed converts a prompt into an embedding vector via POST /embed.
func (c *httpEmbedder) Embed(ctx context.Context, text string) ([]float64, error) {
	body, _ := json.Marshal(embedRequest{Text: text})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/embed", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embedding service unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("embedding service returned %d", resp.StatusCode)
	}

	var out embedResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out.Vector, nil
}

func (c *httpEmbedder) Health(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/health", nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("embedding service unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("embedding service returned %d", resp.StatusCode)
	}
	return nil
}
