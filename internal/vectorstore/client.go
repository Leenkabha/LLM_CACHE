// Package vectorstore is the orchestrator's port to the vector-store service.
//
// The orchestrator depends only on the VectorStore interface. The demo ships
// one adapter -- an HTTP client that speaks the vector-store service's REST
// contract (a FAISS index today) -- but any other implementation (a different
// vector database, an in-process index, a test double) can be plugged in
// without touching the orchestrator. Selecting an adapter is the factory's job
// (see New).
package vectorstore

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

// Match is the nearest stored vector for a query.
type Match struct {
	ID       string  `json:"id"`
	Distance float64 `json:"distance"`
}

// RebuildEntry pairs a previously-saved reply's id with its vector, for
// restoring the index after a restart. Mirrors RebuildEntry in
// vector_store_service/app/main.py exactly -- field names and JSON tags
// must match that contract.
type RebuildEntry struct {
	ID     string    `json:"id"`
	Vector []float64 `json:"vector"`
}

// VectorStore stores embedding vectors and finds the nearest one under a
// configurable similarity threshold.
//
// This is the pluggable seam: the orchestrator holds a VectorStore, so the
// concrete vector database behind it is interchangeable.
type VectorStore interface {
	// Search returns the nearest entry within threshold, or nil on a miss.
	Search(ctx context.Context, vec []float64, topK int, threshold float64) (*Match, error)
	// Upsert stores a vector and returns its generated id.
	Upsert(ctx context.Context, vec []float64) (string, error)
	// Delete removes a vector by id (used by eviction).
	Delete(ctx context.Context, id string) error
	// Size returns the number of stored vectors.
	Size(ctx context.Context) (int, error)
	// Flush clears all stored vectors.
	Flush(ctx context.Context) error
	// Rebuild repopulates the index from durably-stored entries after a restart.
	Rebuild(ctx context.Context, entries []RebuildEntry) (int, error)
	// Health reports whether the underlying vector store is reachable.
	Health(ctx context.Context) error
}

// Backend names the built-in vector-store adapters selectable via configuration.
const (
	// BackendHTTP talks to a remote vector-store service over REST (default).
	BackendHTTP = "http"
)

// registry holds every vector-store adapter, keyed by the name used in
// VECTORSTORE_BACKEND.
var registry = plugin.NewRegistry[VectorStore]("vector store backend")

// Register makes a vector-store adapter available under name.
//
// Call it from an init() in your adapter file. Because adapters live in package
// vectorstore, adding the file is enough -- no existing file changes. Then set
// VECTORSTORE_BACKEND=<name> to select it.
func Register(name string, factory plugin.Factory[VectorStore]) {
	registry.Register(name, factory)
}

// New builds the VectorStore selected by cfg.VectorStoreBackend from the registry.
func New(cfg config.Config) (VectorStore, error) {
	name := cfg.VectorStoreBackend
	if name == "" {
		name = BackendHTTP
	}
	return registry.Build(name, cfg)
}

// The built-in HTTP adapter registers itself.
func init() {
	Register(BackendHTTP, func(cfg config.Config) (VectorStore, error) {
		return NewHTTP(cfg.VectorStoreURL), nil
	})
}

// httpVectorStore is the HTTP adapter for a remote vector-store service. It
// speaks the REST contract only, so the backend (brute-force, FAISS, or another
// vector DB) is interchangeable behind it.
type httpVectorStore struct {
	baseURL string
	http    *http.Client
}

// NewHTTP builds the HTTP vector-store adapter directly. Prefer New for
// config-driven selection; this is exported for tests and explicit wiring.
func NewHTTP(baseURL string) VectorStore {
	return &httpVectorStore{
		baseURL: baseURL,
		http:    &http.Client{Timeout: 10 * time.Second},
	}
}

type searchRequest struct {
	Vector    []float64 `json:"vector"`
	TopK      int       `json:"top_k"`
	Threshold float64   `json:"threshold"`
}

type searchResponse struct {
	Hit  bool    `json:"hit"`
	ID   string  `json:"id"`
	Dist float64 `json:"distance"`
}

func (c *httpVectorStore) Search(ctx context.Context, vec []float64, topK int, threshold float64) (*Match, error) {
	body, _ := json.Marshal(searchRequest{Vector: vec, TopK: topK, Threshold: threshold})
	var out searchResponse
	if err := c.post(ctx, "/search", body, &out); err != nil {
		return nil, err
	}
	if !out.Hit {
		return nil, nil
	}
	return &Match{ID: out.ID, Distance: out.Dist}, nil
}

type upsertRequest struct {
	Vector []float64 `json:"vector"`
}

type upsertResponse struct {
	ID string `json:"id"`
}

func (c *httpVectorStore) Upsert(ctx context.Context, vec []float64) (string, error) {
	body, _ := json.Marshal(upsertRequest{Vector: vec})
	var out upsertResponse
	if err := c.post(ctx, "/upsert", body, &out); err != nil {
		return "", err
	}
	return out.ID, nil
}

// Delete removes a vector by id (used by eviction).
//
// It checks the response status code: a 404 (or any non-200) means the
// deletion did not actually happen, and eviction relies on Delete truly
// removing the vector -- otherwise the vector store and Redis fall out of sync.
func (c *httpVectorStore) Delete(ctx context.Context, id string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.baseURL+"/entries/"+id, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("vector store unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("vector store returned %d", resp.StatusCode)
	}
	return nil
}

type sizeResponse struct {
	Size int `json:"size"`
}

func (c *httpVectorStore) Size(ctx context.Context) (int, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/size", nil)
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	var out sizeResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, err
	}
	return out.Size, nil
}

func (c *httpVectorStore) Flush(ctx context.Context) error {
	return c.post(ctx, "/flush", []byte("{}"), &struct{}{})
}

func (c *httpVectorStore) Health(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/health", nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("vector store unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("vector store returned %d", resp.StatusCode)
	}
	return nil
}

type rebuildRequest struct {
	Entries []RebuildEntry `json:"entries"`
}

type rebuildResponse struct {
	Restored int `json:"restored"`
}

// Rebuild repopulates the RAM-only index from durably-stored entries. The
// vector store has no memory of its own past once its container restarts, so
// the orchestrator replays the surviving Redis entries on startup.
func (c *httpVectorStore) Rebuild(ctx context.Context, entries []RebuildEntry) (int, error) {
	body, _ := json.Marshal(rebuildRequest{Entries: entries})
	var out rebuildResponse
	if err := c.post(ctx, "/rebuild", body, &out); err != nil {
		return 0, err
	}
	return out.Restored, nil
}

func (c *httpVectorStore) post(ctx context.Context, path string, body []byte, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("vector store unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("vector store returned %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
