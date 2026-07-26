// Package plugin provides a small generic registry used by every pluggable
// seam (LLM, embedder, vector store, persistence, queue, eviction policy).
//
// Each seam keeps a package-level Registry[T] and registers its built-in
// implementations from init() functions. A developer adds a new implementation
// by dropping a file into that same package with its own init() that calls the
// seam's Register -- no existing file needs to change. The orchestrator then
// selects one by name at startup via Build.
package plugin

import (
	"fmt"
	"sort"

	"github.com/leenkabha/llm_cache/internal/config"
)

// Factory builds an implementation of a seam from configuration.
//
// Built-in factories read the typed fields they need from config.Config;
// third-party factories are free to read their own settings from the
// environment, so adding one never requires changing config.Config.
type Factory[T any] func(cfg config.Config) (T, error)

// Registry maps a backend name to the Factory that builds it, for one seam.
type Registry[T any] struct {
	kind    string // human-readable seam name, used in error messages
	entries map[string]Factory[T]
}

// NewRegistry creates an empty registry. kind names the seam (e.g. "llm mode")
// and appears in "unknown ..." errors.
func NewRegistry[T any](kind string) *Registry[T] {
	return &Registry[T]{kind: kind, entries: make(map[string]Factory[T])}
}

// Register adds (or replaces) the factory for name. Intended to be called from
// an init() in an adapter file.
func (r *Registry[T]) Register(name string, factory Factory[T]) {
	r.entries[name] = factory
}

// Has reports whether a backend is registered under name.
func (r *Registry[T]) Has(name string) bool {
	_, ok := r.entries[name]
	return ok
}

// Names returns the registered backend names, sorted, for errors and docs.
func (r *Registry[T]) Names() []string {
	names := make([]string, 0, len(r.entries))
	for name := range r.entries {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Build constructs the backend registered under name, or returns a clear error
// listing what is registered. Unknown names fail fast at startup.
func (r *Registry[T]) Build(name string, cfg config.Config) (T, error) {
	factory, ok := r.entries[name]
	if !ok {
		var zero T
		return zero, fmt.Errorf("unknown %s %q (registered: %v)", r.kind, name, r.Names())
	}
	return factory(cfg)
}
