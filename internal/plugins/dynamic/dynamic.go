// Package dynamic provides concurrency-safe indirection for every Go plugin
// interface. Each adapter satisfies the same interface as the implementation it
// fronts and holds the current implementation behind an atomic pointer.
//
// A request loads the pointer once and finishes on whatever it loaded, so a swap
// never interrupts a call in flight; every call that starts after Swap returns
// uses the new implementation. This is the "atomic switch" the plugin manager
// relies on. Nothing here decides WHEN it is safe to swap -- that is the
// manager's job (compatibility checks, state migration, write gating).
package dynamic

import (
	"context"
	"errors"
	"io"
	"sync/atomic"

	"github.com/leenkabha/llm_cache/internal/embedder"
	"github.com/leenkabha/llm_cache/internal/llm"
	"github.com/leenkabha/llm_cache/internal/persistence"
	"github.com/leenkabha/llm_cache/internal/vectorstore"
)

// ErrUnsupported is returned by an optional operation the current
// implementation does not provide.
var ErrUnsupported = errors.New("operation not supported by the active implementation")

// Close releases an implementation's resources if it has any.
func Close(impl any) {
	if c, ok := impl.(io.Closer); ok {
		_ = c.Close()
		return
	}
	if c, ok := impl.(interface{ Close() }); ok {
		c.Close()
	}
}

// ---- LLM ---------------------------------------------------------------------

type llmHolder struct{ llm.Backend }

// LLM is a swappable llm.Backend.
type LLM struct{ cur atomic.Pointer[llmHolder] }

var (
	_ llm.Backend       = (*LLM)(nil)
	_ llm.UsageReporter = (*LLM)(nil)
)

func NewLLM(initial llm.Backend) *LLM {
	d := &LLM{}
	d.cur.Store(&llmHolder{initial})
	return d
}

func (d *LLM) Current() llm.Backend { return d.cur.Load().Backend }
func (d *LLM) Swap(next llm.Backend) llm.Backend {
	return d.cur.Swap(&llmHolder{next}).Backend
}
func (d *LLM) Complete(ctx context.Context, prompt string) (string, error) {
	return d.cur.Load().Complete(ctx, prompt)
}

// Usage forwards to the active backend when it meters usage.
func (d *LLM) Usage() (llm.UsageSnapshot, bool) {
	if r, ok := d.cur.Load().Backend.(llm.UsageReporter); ok {
		return r.Usage()
	}
	return llm.UsageSnapshot{}, false
}

// Health forwards to the active backend when it can report health.
func (d *LLM) Health(ctx context.Context) error {
	if h, ok := d.cur.Load().Backend.(interface{ Health(context.Context) error }); ok {
		return h.Health(ctx)
	}
	return nil
}

// ---- Embedder ----------------------------------------------------------------

type embHolder struct{ embedder.Embedder }

// Embedder is a swappable embedder.Embedder.
type Embedder struct{ cur atomic.Pointer[embHolder] }

var (
	_ embedder.Embedder          = (*Embedder)(nil)
	_ embedder.ModelInfoProvider = (*Embedder)(nil)
)

func NewEmbedder(initial embedder.Embedder) *Embedder {
	d := &Embedder{}
	d.cur.Store(&embHolder{initial})
	return d
}

func (d *Embedder) Current() embedder.Embedder { return d.cur.Load().Embedder }
func (d *Embedder) Swap(next embedder.Embedder) embedder.Embedder {
	return d.cur.Swap(&embHolder{next}).Embedder
}
func (d *Embedder) Embed(ctx context.Context, text string) ([]float64, error) {
	return d.cur.Load().Embed(ctx, text)
}
func (d *Embedder) Health(ctx context.Context) error { return d.cur.Load().Health(ctx) }
func (d *Embedder) ModelInfo(ctx context.Context) (embedder.ModelInfo, error) {
	if m, ok := d.cur.Load().Embedder.(embedder.ModelInfoProvider); ok {
		return m.ModelInfo(ctx)
	}
	return embedder.ModelInfo{}, ErrUnsupported
}

// ---- VectorStore -------------------------------------------------------------

type vsHolder struct{ vectorstore.VectorStore }

// VectorStore is a swappable vectorstore.VectorStore.
type VectorStore struct{ cur atomic.Pointer[vsHolder] }

var (
	_ vectorstore.VectorStore  = (*VectorStore)(nil)
	_ vectorstore.InfoProvider = (*VectorStore)(nil)
)

func NewVectorStore(initial vectorstore.VectorStore) *VectorStore {
	d := &VectorStore{}
	d.cur.Store(&vsHolder{initial})
	return d
}

func (d *VectorStore) Current() vectorstore.VectorStore { return d.cur.Load().VectorStore }
func (d *VectorStore) Swap(next vectorstore.VectorStore) vectorstore.VectorStore {
	return d.cur.Swap(&vsHolder{next}).VectorStore
}
func (d *VectorStore) Search(ctx context.Context, v []float64, k int, th float64) ([]vectorstore.Match, error) {
	return d.cur.Load().Search(ctx, v, k, th)
}
func (d *VectorStore) Upsert(ctx context.Context, v []float64) (string, error) {
	return d.cur.Load().Upsert(ctx, v)
}
func (d *VectorStore) Delete(ctx context.Context, id string) error {
	return d.cur.Load().Delete(ctx, id)
}
func (d *VectorStore) Size(ctx context.Context) (int, error) { return d.cur.Load().Size(ctx) }
func (d *VectorStore) Flush(ctx context.Context) error       { return d.cur.Load().Flush(ctx) }
func (d *VectorStore) Rebuild(ctx context.Context, e []vectorstore.RebuildEntry) (int, error) {
	return d.cur.Load().Rebuild(ctx, e)
}
func (d *VectorStore) Health(ctx context.Context) error { return d.cur.Load().Health(ctx) }
func (d *VectorStore) Info(ctx context.Context) (vectorstore.Info, error) {
	if i, ok := d.cur.Load().VectorStore.(vectorstore.InfoProvider); ok {
		return i.Info(ctx)
	}
	return vectorstore.Info{}, ErrUnsupported
}

// ---- Persistence -------------------------------------------------------------

type storeHolder struct{ persistence.Store }

// Store is a swappable persistence.Store.
type Store struct{ cur atomic.Pointer[storeHolder] }

var (
	_ persistence.Store          = (*Store)(nil)
	_ persistence.DetailedLoader = (*Store)(nil)
)

func NewStore(initial persistence.Store) *Store {
	d := &Store{}
	d.cur.Store(&storeHolder{initial})
	return d
}

func (d *Store) Current() persistence.Store { return d.cur.Load().Store }
func (d *Store) Swap(next persistence.Store) persistence.Store {
	return d.cur.Swap(&storeHolder{next}).Store
}
func (d *Store) Save(e persistence.Entry) error { return d.cur.Load().Save(e) }
func (d *Store) Load(id string) (persistence.Entry, bool) {
	return d.cur.Load().Load(id)
}
func (d *Store) List() ([]persistence.Entry, error) { return d.cur.Load().List() }
func (d *Store) Size() (int, error)                 { return d.cur.Load().Size() }
func (d *Store) Delete(id string) error             { return d.cur.Load().Delete(id) }
func (d *Store) Flush() error                       { return d.cur.Load().Flush() }
func (d *Store) Health() error                      { return d.cur.Load().Health() }
func (d *Store) LoadDetailed(id string) (persistence.Entry, error) {
	if l, ok := d.cur.Load().Store.(persistence.DetailedLoader); ok {
		return l.LoadDetailed(id)
	}
	e, ok := d.cur.Load().Load(id)
	if !ok {
		return persistence.Entry{}, persistence.ErrNotFound
	}
	return e, nil
}
