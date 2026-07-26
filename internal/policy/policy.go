// Package policy holds pluggable cache admission/eviction policy state.
package policy

import (
	"fmt"

	"github.com/leenkabha/llm_cache/internal/config"
	"github.com/leenkabha/llm_cache/internal/plugin"
)

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

// Policy names the built-in eviction policies selectable via configuration.
const (
	// PolicyLRU evicts the least-recently-used entry (default).
	PolicyLRU = "lru"
	// PolicyLFU evicts the least-frequently-used entry.
	PolicyLFU = "lfu"
)

// registry holds every eviction policy, keyed by the name used in CACHE_POLICY.
var registry = plugin.NewRegistry[EvictionPolicy]("eviction policy")

// Register makes an eviction policy available under name.
//
// Call it from an init() in your adapter file. Because adapters live in package
// policy, adding the file is enough -- no existing file changes. Then set
// CACHE_POLICY=<name> to select it.
func Register(name string, factory plugin.Factory[EvictionPolicy]) {
	registry.Register(name, factory)
}

// The built-in policies register themselves. They ignore config -- a policy's
// state is internal -- but keep the uniform factory signature.
func init() {
	Register(PolicyLRU, func(config.Config) (EvictionPolicy, error) { return newLRUPolicy(), nil })
	Register(PolicyLFU, func(config.Config) (EvictionPolicy, error) { return newLFUPolicy(), nil })
}

type Manager struct {
	policy EvictionPolicy
}

// New builds a policy Manager for cfg.Policy.
func New(cfg config.Config) (*Manager, error) {
	return NewManager(cfg.Policy)
}

// NewManager builds a policy Manager for the named eviction policy.
//
// Unknown names are a configuration error, so a typo fails fast at startup
// instead of silently defaulting.
func NewManager(name string) (*Manager, error) {
	if name == "" {
		name = PolicyLRU
	}
	p, err := registry.Build(name, config.Config{})
	if err != nil {
		return nil, err
	}
	return &Manager{policy: p}, nil
}

// Current returns the active policy name.
func (m *Manager) Current() string {
	return m.policy.Name()
}

// Set validates a requested policy.
//
// Runtime policy switching is intentionally not supported because each concrete
// policy owns different metadata. To change policy, set CACHE_POLICY before
// startup and restart the service.
func (m *Manager) Set(name string) error {
	if !registry.Has(name) {
		return fmt.Errorf("invalid policy %q (registered: %v)", name, registry.Names())
	}
	if name == m.Current() {
		return nil
	}
	return fmt.Errorf("runtime policy switching is not supported; set CACHE_POLICY=%s and restart", name)
}

// OnHit records that an existing cache entry was used. LRU updates recency here;
// LFU updates frequency.
func (m *Manager) OnHit(id string) { m.policy.OnHit(id) }

// OnInsert records that a new cache entry was admitted.
func (m *Manager) OnInsert(id string) { m.policy.OnInsert(id) }

// OnDelete removes any policy-owned metadata for a cache entry.
func (m *Manager) OnDelete(id string) { m.policy.OnDelete(id) }

// Victim returns the next entry id to evict when capacity is exceeded.
func (m *Manager) Victim() (string, bool) { return m.policy.Victim() }

// Flush clears policy-owned metadata.
func (m *Manager) Flush() { m.policy.Flush() }
