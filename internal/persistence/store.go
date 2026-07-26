// Package persistence stores cache entries keyed by entry id.
// The HLD uses Redis as the source of truth; v0 ships an in-memory store
// behind the same interface so the orchestrator logic is already correct.
package persistence

import (
	"sync"
	"time"

	"github.com/leenkabha/llm_cache/internal/config"
	"github.com/leenkabha/llm_cache/internal/plugin"
)

// Backend names the built-in persistence adapters selectable via configuration.
const (
	// BackendRedis persists durably in Redis (default, HLD source of truth).
	BackendRedis = "redis"
	// BackendMemory keeps entries in process memory (tests / local fallback).
	BackendMemory = "memory"
)

// registry holds every persistence adapter, keyed by the name used in
// PERSISTENCE_BACKEND.
var registry = plugin.NewRegistry[Store]("persistence backend")

// Register makes a persistence adapter available under name.
//
// Call it from an init() in your adapter file. Because adapters live in package
// persistence, adding the file is enough -- no existing file changes. Then set
// PERSISTENCE_BACKEND=<name> to select it.
func Register(name string, factory plugin.Factory[Store]) {
	registry.Register(name, factory)
}

// New builds the Store selected by cfg.PersistenceBackend from the registry.
func New(cfg config.Config) (Store, error) {
	name := cfg.PersistenceBackend
	if name == "" {
		name = BackendRedis
	}
	return registry.Build(name, cfg)
}

// The built-in stores register themselves.
func init() {
	Register(BackendRedis, func(cfg config.Config) (Store, error) {
		return NewRedisStore(cfg.RedisAddr)
	})
	Register(BackendMemory, func(config.Config) (Store, error) {
		return NewMemoryStore(), nil
	})
}

// Entry is the policy-agnostic cache record.
//
// It intentionally contains only the core data needed to restore and serve a
// cached response. Eviction metadata belongs to the active policy
// implementation, so LRU/LFU can remain pluggable and policy can be changed on
// restart without changing the cache schema.
type Entry struct {
	ID        string    `json:"id"`
	Prompt    string    `json:"prompt"`
	Reply     string    `json:"reply"`
	Vector    []float64 `json:"vector"`
	CreatedAt time.Time `json:"created_at"`
}

// Store maps a vector-store entry id to its policy-agnostic cache entry.
type Store interface {
	Save(entry Entry) error
	Load(id string) (Entry, bool)
	List() ([]Entry, error)
	Size() (int, error)
	Delete(id string) error
	Flush() error
	Health() error
}

// MemoryStore is an in-memory implementation used by tests and local fallback
// scenarios. Production orchestration uses RedisStore.
type MemoryStore struct {
	mu      sync.RWMutex
	entries map[string]Entry
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{entries: make(map[string]Entry)}
}

func (m *MemoryStore) Save(entry Entry) error {
	m.mu.Lock()
	entry.Vector = cloneVector(entry.Vector)
	m.entries[entry.ID] = entry
	m.mu.Unlock()
	return nil
}

func (m *MemoryStore) Load(id string) (Entry, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	entry, ok := m.entries[id]
	entry.Vector = cloneVector(entry.Vector)
	return entry, ok
}

func (m *MemoryStore) List() ([]Entry, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	entries := make([]Entry, 0, len(m.entries))
	for _, entry := range m.entries {
		entry.Vector = cloneVector(entry.Vector)
		entries = append(entries, entry)
	}
	return entries, nil
}

func (m *MemoryStore) Size() (int, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.entries), nil
}

func (m *MemoryStore) Delete(id string) error {
	m.mu.Lock()
	delete(m.entries, id)
	m.mu.Unlock()
	return nil
}

func (m *MemoryStore) Flush() error {
	m.mu.Lock()
	m.entries = make(map[string]Entry)
	m.mu.Unlock()
	return nil
}

func (m *MemoryStore) Health() error {
	return nil
}

func cloneVector(vec []float64) []float64 {
	if vec == nil {
		return nil
	}
	out := make([]float64, len(vec))
	copy(out, vec)
	return out
}
