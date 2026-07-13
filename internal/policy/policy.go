// Package policy holds pluggable cache admission/eviction policy state.
package policy

import "fmt"

// EvictionPolicy is the contract implemented by policy-specific metadata.
//
// Cache entries stay policy-agnostic. LRU/LFU own their own access metadata
// behind this interface, so changing policy on restart does not require changing
// the persistence schema.
type EvictionPolicy interface {
	Name() string
	OnHit(id string)
	OnInsert(id string)
	OnDelete(id string)
	Victim() (string, bool)
	Flush()
}

type Manager struct {
	policy EvictionPolicy
}

func New(initial string) *Manager {
	if !valid(initial) {
		initial = "lru"
	}
	return &Manager{policy: newPolicy(initial)}
}

func valid(name string) bool {
	return name == "lru" || name == "lfu"
}

func newPolicy(name string) EvictionPolicy {
	switch name {
	case "lfu":
		return newLFUPolicy()
	default:
		return newLRUPolicy()
	}
}

// Current returns the active policy name.
func (m *Manager) Current() string {
	return m.policy.Name()
}

// Set validates a requested policy.
//
// Runtime policy switching is intentionally not supported because each
// concrete policy owns different metadata. To change policy, set CACHE_POLICY
// before startup and restart the service.
func (m *Manager) Set(name string) error {
	if !valid(name) {
		return fmt.Errorf("invalid policy %q (want lru or lfu)", name)
	}
	if name == m.Current() {
		return nil
	}
	return fmt.Errorf("runtime policy switching is not supported; set CACHE_POLICY=%s and restart", name)
}

// OnHit records that an existing cache entry was used.
//
// The manager exposes this hook so the orchestrator does not need to know how a
// specific eviction policy tracks access. LRU updates recency here, while LFU
// updates frequency.
func (m *Manager) OnHit(id string) {
	m.policy.OnHit(id)
}

// OnInsert records that a new cache entry was admitted.
func (m *Manager) OnInsert(id string) {
	m.policy.OnInsert(id)
}

// OnDelete removes any policy-owned metadata for a cache entry.
func (m *Manager) OnDelete(id string) {
	m.policy.OnDelete(id)
}

// Victim returns the next entry id to evict when capacity is exceeded.
//
// Concrete policies will implement the actual selection logic. LRU will return
// the least-recently used id; LFU will return the least-frequently used id.
func (m *Manager) Victim() (string, bool) {
	return m.policy.Victim()
}

// Flush clears policy-owned metadata.
func (m *Manager) Flush() {
	m.policy.Flush()
}
