// Package policy holds cache admission/eviction policy state.
// v0 tracks the selected policy and validates switches; the actual
// LRU/LFU victim selection lands in week 9.
package policy

import (
	"fmt"
	"sync"
)

type Manager struct {
	mu      sync.RWMutex
	current string
}

func New(initial string) *Manager {
	if !valid(initial) {
		initial = "lru"
	}
	return &Manager{current: initial}
}

func valid(name string) bool {
	return name == "lru" || name == "lfu"
}

// Current returns the active policy name.
func (m *Manager) Current() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.current
}

// Set switches the active policy, rejecting unknown names.
func (m *Manager) Set(name string) error {
	if !valid(name) {
		return fmt.Errorf("invalid policy %q (want lru or lfu)", name)
	}
	m.mu.Lock()
	m.current = name
	m.mu.Unlock()
	return nil
}
