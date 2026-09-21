package manager

import (
	"errors"
	"fmt"

	"github.com/leenkabha/llm_cache/internal/cachequeue"
	"github.com/leenkabha/llm_cache/internal/embedder"
	"github.com/leenkabha/llm_cache/internal/llm"
	"github.com/leenkabha/llm_cache/internal/persistence"
	"github.com/leenkabha/llm_cache/internal/plugins"
	"github.com/leenkabha/llm_cache/internal/plugins/dynamic"
	"github.com/leenkabha/llm_cache/internal/policy"
	"github.com/leenkabha/llm_cache/internal/vectorstore"
)

// slotTypes are the six orchestrator seams. The three Python plugin types are
// activated into the embedder and vector-store slots.
var slotTypes = []plugins.Type{
	plugins.TypeLLM, plugins.TypeEmbedder, plugins.TypeVectorStore,
	plugins.TypePersistence, plugins.TypeQueue, plugins.TypePolicy,
}

// Slots is the set of swappable adapters the orchestrator runs on, plus the
// built-in (environment-selected) implementation of each, which is what a slot
// returns to when its plugin is deactivated.
type Slots struct {
	LLM         *dynamic.LLM
	Embedder    *dynamic.Embedder
	VectorStore *dynamic.VectorStore
	Store       *dynamic.Store
	Queue       *dynamic.Queue
	Policy      *policy.Manager

	baseline map[plugins.Type]any
}

// NewSlots wraps the built-in implementations. Give the orchestrator the
// returned adapters (not the originals) so every later swap is visible to it.
func NewSlots(l llm.Backend, e embedder.Embedder, v vectorstore.VectorStore, s persistence.Store, q cachequeue.Queue, p *policy.Manager) *Slots {
	return &Slots{
		LLM: dynamic.NewLLM(l), Embedder: dynamic.NewEmbedder(e), VectorStore: dynamic.NewVectorStore(v),
		Store: dynamic.NewStore(s), Queue: dynamic.NewQueue(q), Policy: p,
		baseline: map[plugins.Type]any{
			plugins.TypeLLM: l, plugins.TypeEmbedder: e, plugins.TypeVectorStore: v,
			plugins.TypePersistence: s, plugins.TypeQueue: q, plugins.TypePolicy: p.Active(),
		},
	}
}

func (s *Slots) builtin(slot plugins.Type) any { return s.baseline[slot] }

// current returns the implementation presently behind a slot.
func (s *Slots) current(slot plugins.Type) any {
	switch slot {
	case plugins.TypeLLM:
		return s.LLM.Current()
	case plugins.TypeEmbedder:
		return s.Embedder.Current()
	case plugins.TypeVectorStore:
		return s.VectorStore.Current()
	case plugins.TypePersistence:
		return s.Store.Current()
	case plugins.TypeQueue:
		return s.Queue.Current()
	case plugins.TypePolicy:
		return s.Policy.Active()
	}
	return nil
}

// swap installs impl behind slot and returns what it replaced.
func (s *Slots) swap(slot plugins.Type, impl any) (any, error) {
	bad := func() (any, error) {
		return nil, fmt.Errorf("adapter %T does not implement the %s contract", impl, slot)
	}
	switch slot {
	case plugins.TypeLLM:
		if v, ok := impl.(llm.Backend); ok {
			return s.LLM.Swap(v), nil
		}
	case plugins.TypeEmbedder:
		if v, ok := impl.(embedder.Embedder); ok {
			return s.Embedder.Swap(v), nil
		}
	case plugins.TypeVectorStore:
		if v, ok := impl.(vectorstore.VectorStore); ok {
			return s.VectorStore.Swap(v), nil
		}
	case plugins.TypePersistence:
		if v, ok := impl.(persistence.Store); ok {
			return s.Store.Swap(v), nil
		}
	case plugins.TypeQueue:
		if v, ok := impl.(cachequeue.Queue); ok {
			return s.Queue.Swap(v), nil
		}
	case plugins.TypePolicy:
		if v, ok := impl.(policy.EvictionPolicy); ok {
			return s.Policy.Replace(v), nil
		}
	default:
		return nil, errors.New("unknown slot")
	}
	return bad()
}

// stateBearing slots hold cache state, so a failure to restore them must not
// silently fall back to a different (stale) built-in.
func stateBearing(slot plugins.Type) bool {
	switch slot {
	case plugins.TypeEmbedder, plugins.TypeVectorStore, plugins.TypePersistence:
		return true
	}
	return false
}
