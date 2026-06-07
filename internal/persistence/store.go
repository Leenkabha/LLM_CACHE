// Package persistence stores cached replies and metadata keyed by entry id.
// The HLD uses Redis as the source of truth; v0 ships an in-memory store
// behind the same interface so the orchestrator logic is already correct.
package persistence

import "sync"

// Store maps a vector-store entry id to its cached reply.
type Store interface {
	Save(id, reply string) error
	Load(id string) (string, bool)
	Delete(id string) error
	Flush() error
}

// MemoryStore is the v0 in-memory implementation.
// TODO(week 8): add a RedisStore implementing this same interface.
type MemoryStore struct {
	mu      sync.RWMutex
	replies map[string]string
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{replies: make(map[string]string)}
}

func (m *MemoryStore) Save(id, reply string) error {
	m.mu.Lock()
	m.replies[id] = reply
	m.mu.Unlock()
	return nil
}

func (m *MemoryStore) Load(id string) (string, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	reply, ok := m.replies[id]
	return reply, ok
}

func (m *MemoryStore) Delete(id string) error {
	m.mu.Lock()
	delete(m.replies, id)
	m.mu.Unlock()
	return nil
}

func (m *MemoryStore) Flush() error {
	m.mu.Lock()
	m.replies = make(map[string]string)
	m.mu.Unlock()
	return nil
}
