package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/leenkabha/llm_cache/internal/cachequeue"
	"github.com/leenkabha/llm_cache/internal/config"
	"github.com/leenkabha/llm_cache/internal/persistence"
	"github.com/leenkabha/llm_cache/internal/policy"
	"github.com/leenkabha/llm_cache/internal/vectorstore"
)

// fakeVectorStore is a real (if tiny) in-memory nearest-neighbour index, used
// to exercise orchestrator-level consistency and eviction behavior without a
// live FAISS service. Unlike fixedSearch (a canned-response stub), it lets
// upsert/search/delete actually interact, and its Upsert/Delete calls can be
// made to fail on demand to simulate a partial-failure window between the
// vector store and Redis.
type fakeVectorStore struct {
	mu      sync.Mutex
	vectors map[string][]float64
	nextID  int

	failNextUpsert error
	failDeleteIDs  map[string]error
}

func newFakeVectorStore() *fakeVectorStore {
	return &fakeVectorStore{vectors: map[string][]float64{}, failDeleteIDs: map[string]error{}}
}

func (f *fakeVectorStore) Upsert(_ context.Context, vec []float64) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failNextUpsert != nil {
		err := f.failNextUpsert
		f.failNextUpsert = nil
		return "", err
	}
	f.nextID++
	id := fmt.Sprintf("id-%d", f.nextID)
	f.vectors[id] = vec
	return id, nil
}

func (f *fakeVectorStore) Delete(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err, ok := f.failDeleteIDs[id]; ok {
		delete(f.failDeleteIDs, id) // fail once, then let a retry succeed
		return err
	}
	delete(f.vectors, id)
	return nil
}

func (f *fakeVectorStore) Search(_ context.Context, vec []float64, topK int, threshold float64) ([]vectorstore.Match, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var matches []vectorstore.Match
	for id, stored := range f.vectors {
		d := sqDist(vec, stored)
		if d <= threshold {
			matches = append(matches, vectorstore.Match{ID: id, Distance: d})
		}
	}
	// simple insertion sort by distance; test data sets are tiny
	for i := 1; i < len(matches); i++ {
		for j := i; j > 0 && matches[j].Distance < matches[j-1].Distance; j-- {
			matches[j], matches[j-1] = matches[j-1], matches[j]
		}
	}
	if len(matches) > topK {
		matches = matches[:topK]
	}
	return matches, nil
}

func (f *fakeVectorStore) Size(context.Context) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.vectors), nil
}

func (f *fakeVectorStore) Flush(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.vectors = map[string][]float64{}
	return nil
}

func (f *fakeVectorStore) Rebuild(_ context.Context, entries []vectorstore.RebuildEntry) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.vectors = map[string][]float64{}
	for _, e := range entries {
		f.vectors[e.ID] = e.Vector
	}
	return len(entries), nil
}

func (f *fakeVectorStore) Health(context.Context) error { return nil }

func (f *fakeVectorStore) has(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.vectors[id]
	return ok
}

func sqDist(a, b []float64) float64 {
	var sum float64
	for i := range a {
		d := a[i] - b[i]
		sum += d * d
	}
	return sum
}

// failingSaveStore wraps a MemoryStore and fails the next Save call once,
// simulating Redis being unreachable at the moment a fresh cache-miss result
// is being persisted (Case B: vector store succeeded, Redis persistence fails).
type failingSaveStore struct {
	*persistence.MemoryStore
	failNextSave error
}

func (s *failingSaveStore) Save(entry persistence.Entry) error {
	if s.failNextSave != nil {
		err := s.failNextSave
		s.failNextSave = nil
		return err
	}
	return s.MemoryStore.Save(entry)
}

func newConsistencyService(t *testing.T, vstore *fakeVectorStore, store persistence.Store, pol *policy.Manager, capacity int) *Service {
	t.Helper()
	if pol == nil {
		var err error
		pol, err = policy.NewManager("lru")
		if err != nil {
			t.Fatal(err)
		}
	}
	return &Service{
		cfg:    config.Config{TopK: 1, Threshold: 0.01, Capacity: capacity},
		embed:  fixedEmbedder{},
		vstore: vstore,
		store:  store,
		policy: pol,
		llm:    &countingLLM{},
		queue:  &countingQueue{},
	}
}

// --- Case B: vector store upsert succeeds, Redis persistence fails --------

func TestUpdateCacheCompensatesWhenSaveFails(t *testing.T) {
	vstore := newFakeVectorStore()
	store := &failingSaveStore{MemoryStore: persistence.NewMemoryStore(), failNextSave: errors.New("redis unreachable")}
	s := newConsistencyService(t, vstore, store, nil, 0)

	err := s.updateCache(context.Background(), "prompt", []float64{1, 0}, "reply")
	if err == nil {
		t.Fatal("updateCache() with failing Save returned nil error")
	}
	if size, _ := vstore.Size(context.Background()); size != 0 {
		t.Fatalf("vector store size = %d after compensating delete, want 0 (no orphan vector)", size)
	}
}

// Documents the residual risk when the compensating delete ITSELF fails: the
// vector is left orphaned in the vector store with no backing Redis entry.
// The system tolerates this at query time (handleQuery skips vector matches
// with no backing store entry -- see the "cache inconsistency" log line) but
// the orphan vector is never cleaned up automatically.
func TestUpdateCacheDoubleFailureLeavesOrphanVector(t *testing.T) {
	vstore := newFakeVectorStore()
	store := &failingSaveStore{MemoryStore: persistence.NewMemoryStore(), failNextSave: errors.New("redis unreachable")}
	s := newConsistencyService(t, vstore, store, nil, 0)

	// Grab the id FAISS will assign so we can make its compensating Delete fail too.
	vstore.mu.Lock()
	vstore.failDeleteIDs["id-1"] = errors.New("vector store also unreachable")
	vstore.mu.Unlock()

	if err := s.updateCache(context.Background(), "prompt", []float64{1, 0}, "reply"); err == nil {
		t.Fatal("expected error")
	}
	if !vstore.has("id-1") {
		t.Fatal("expected orphan vector id-1 to remain after double failure (documents current behavior)")
	}
	// The orphan must not be servable: a search would return it, but the
	// missing store entry must cause handleQuery to skip it, not crash or
	// serve a broken reply. Verify at the Load layer directly.
	if _, ok := store.Load("id-1"); ok {
		t.Fatal("persistence unexpectedly has an entry for the orphaned vector id")
	}
}

// --- Case C: Redis delete succeeds during eviction, vector-store delete fails ---

func TestEnforceCapacityEvictionCaseC(t *testing.T) {
	vstore := newFakeVectorStore()
	store := persistence.NewMemoryStore()
	pol, err := policy.NewManager("lru")
	if err != nil {
		t.Fatal(err)
	}
	s := newConsistencyService(t, vstore, store, pol, 1)

	ctx := context.Background()
	if err := s.updateCache(ctx, "first", []float64{1, 0}, "reply-1"); err != nil {
		t.Fatal(err)
	}
	if s.evictions != 0 {
		t.Fatalf("evictions=%d after first insert, want 0 (capacity not yet exceeded)", s.evictions)
	}
	// The eviction victim will be "id-1" (LRU, oldest). Make its vector-store
	// deletion fail once so Redis deletion succeeds but the vector doesn't.
	vstore.mu.Lock()
	vstore.failDeleteIDs["id-1"] = errors.New("vector store unreachable")
	vstore.mu.Unlock()

	err = s.updateCache(ctx, "second", []float64{0, 1}, "reply-2")
	if err == nil {
		t.Fatal("expected enforceCapacity to surface the vector-store delete failure")
	}
	if s.evictions != 0 {
		t.Fatalf("evictions=%d after failed eviction attempt, want 0 (delete did not fully succeed)", s.evictions)
	}

	// Document the inconsistency window: Redis no longer has "id-1"...
	if _, ok := store.Load("id-1"); ok {
		t.Fatal("expected id-1 removed from persistence")
	}
	// ...but the vector-store still does, and policy metadata was never
	// cleared for it (OnDelete is only called after both deletes succeed).
	if !vstore.has("id-1") {
		t.Fatal("expected id-1 to still be present in the vector store (documents Case C)")
	}
	if victim, ok := pol.Victim(); !ok || victim != "id-1" {
		t.Fatalf("policy victim = (%q, %v), want stale id-1 still tracked (documents Case C)", victim, ok)
	}

	// Self-healing: the next capacity enforcement pass (triggered by another
	// insert) retries the same victim and, once the vector store is healthy
	// again, finishes the eviction cleanly.
	if err := s.updateCache(ctx, "third", []float64{0, 0}, "reply-3"); err != nil {
		t.Fatalf("self-healing retry failed: %v", err)
	}
	if vstore.has("id-1") {
		t.Fatal("expected id-1 fully evicted from the vector store after self-healing retry")
	}
	// Two evictions, not one: the retry first completes id-1's interrupted
	// eviction (its persistence entry was already gone, but it was still
	// orphaned in the vector store and policy metadata, so store.Delete is a
	// harmless no-op and vstore.Delete/OnDelete finish the job -- counted
	// once). Persistence size is still above capacity afterward (id-1 was
	// already absent from it), so enforceCapacity's loop evicts a second,
	// genuine victim (id-2) in the same pass.
	if s.evictions != 2 {
		t.Fatalf("evictions=%d after self-healing retry, want 2 (stale id-1 completion + genuine id-2 eviction)", s.evictions)
	}
	if _, ok := pol.Victim(); ok {
		size, _ := store.Size()
		if size > 1 {
			t.Fatalf("policy still tracks a victim after self-healing; persistence size=%d", size)
		}
	}
}

// --- Section 11: eviction must remove an entry from every store, and the
// evicted prompt must not resurface as a stale semantic hit. -------------

func TestEvictionSynchronizesAllStoresAndStopsSemanticHits(t *testing.T) {
	vstore := newFakeVectorStore()
	store := persistence.NewMemoryStore()
	pol, err := policy.NewManager("lru")
	if err != nil {
		t.Fatal(err)
	}
	s := newConsistencyService(t, vstore, store, pol, 1)
	ctx := context.Background()

	firstVec := []float64{1, 0}
	if err := s.updateCache(ctx, "first prompt", firstVec, "reply-1"); err != nil {
		t.Fatal(err)
	}
	if err := s.updateCache(ctx, "second prompt", []float64{0, 1}, "reply-2"); err != nil {
		t.Fatal(err)
	}

	// capacity=1 must have evicted "first prompt"'s entry (id-1) everywhere.
	if s.evictions != 1 {
		t.Fatalf("evictions=%d, want 1", s.evictions)
	}
	if _, ok := store.Load("id-1"); ok {
		t.Fatal("evicted entry still present in persistence")
	}
	if vstore.has("id-1") {
		t.Fatal("evicted entry still present in the vector store")
	}
	if victim, ok := pol.Victim(); ok && victim == "id-1" {
		t.Fatal("evicted entry still tracked in policy metadata")
	}

	// A query with the exact vector of the evicted entry must now miss and
	// fall through to the LLM, not resurrect a stale hit.
	llm := s.llm.(*countingLLM)
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/query", strings.NewReader(`{"prompt":"first prompt"}`)))
	if rec.Code != 200 {
		t.Fatalf("status=%d", rec.Code)
	}
	if llm.calls != 1 {
		t.Fatalf("llm.calls=%d, want 1 (evicted prompt must miss, not hit stale data)", llm.calls)
	}
}

// --- Section 9: cache stampede -------------------------------------------

// threadsafeCountingLLM and threadsafeEmptySearch are dedicated, mutex-safe
// test doubles for the concurrency test below. The existing countingLLM /
// fixedSearch / countingQueue helpers in service_test.go are plain structs
// with no synchronization -- fine for the sequential tests they were written
// for, but using them under concurrent load makes -race flag the STUB's
// unsynchronized fields, which would misreport a race in this test harness
// as a race in the orchestrator itself. These wrappers isolate that.
type threadsafeCountingLLM struct {
	mu    sync.Mutex
	calls int
}

func (l *threadsafeCountingLLM) Complete(context.Context, string) (string, error) {
	l.mu.Lock()
	l.calls++
	l.mu.Unlock()
	return "fresh reply", nil
}

func (l *threadsafeCountingLLM) callCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.calls
}

type threadsafeEmptySearch struct{ vectorstore.VectorStore }

func (threadsafeEmptySearch) Search(context.Context, []float64, int, float64) ([]vectorstore.Match, error) {
	return nil, nil
}

type threadsafeCountingQueue struct {
	mu   sync.Mutex
	jobs int
}

func (q *threadsafeCountingQueue) Enqueue(context.Context, cachequeue.Job) error {
	q.mu.Lock()
	q.jobs++
	q.mu.Unlock()
	return nil
}

func (q *threadsafeCountingQueue) Run(context.Context, func(context.Context, cachequeue.Job) error) {}

// Documents current behavior: because a cache-miss response is returned to
// the client BEFORE the async cache-update job is processed, concurrent
// identical requests that all arrive during that window each independently
// miss and each call the LLM. There is no in-flight request de-duplication.
func TestConcurrentIdenticalMissesEachCallTheLLM(t *testing.T) {
	const n = 12
	llm := &threadsafeCountingLLM{}
	queue := &threadsafeCountingQueue{}
	s := &Service{
		cfg:    config.Config{TopK: 1, Threshold: 0.25},
		embed:  fixedEmbedder{},
		vstore: threadsafeEmptySearch{}, // always empty -> always a miss
		store:  persistence.NewMemoryStore(),
		policy: mustPolicy(t),
		llm:    llm,
		queue:  queue,
	}

	var wg sync.WaitGroup
	var okCount int64
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := httptest.NewRecorder()
			s.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/query", strings.NewReader(`{"prompt":"stampede"}`)))
			if rec.Code == 200 {
				atomic.AddInt64(&okCount, 1)
			}
		}()
	}
	wg.Wait()

	if int(okCount) != n {
		t.Fatalf("ok responses=%d, want %d", okCount, n)
	}
	if got := llm.callCount(); got != n {
		t.Fatalf("llm.calls=%d, want %d -- current design has no stampede protection; "+
			"if this ever drops below %d, the code changed to dedupe in-flight misses "+
			"and this test documents that improvement instead", got, n, n)
	}
}

func mustPolicy(t *testing.T) *policy.Manager {
	t.Helper()
	pol, err := policy.NewManager("lru")
	if err != nil {
		t.Fatal(err)
	}
	return pol
}

// countingQueue, countingLLM, fixedEmbedder, and fixedSearch are defined in
// service_test.go and reused here.

// --- /health must not let one slow dependency starve the budget for the next ---

type slowHealthEmbedder struct {
	fixedEmbedder
	delay time.Duration
}

func (e slowHealthEmbedder) Health(ctx context.Context) error {
	select {
	case <-time.After(e.delay):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type needsTimeVectorStore struct {
	*fakeVectorStore
	need time.Duration
}

func (v needsTimeVectorStore) Health(ctx context.Context) error {
	select {
	case <-time.After(v.need):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Regression test: previously /health ran all three dependency checks against
// one shared 3s context.WithTimeout. A slow-but-eventually-healthy first
// check (embedding, here 2.5s) could leave the next check (vector store, here
// needing 1s) with almost no time left, making a perfectly healthy dependency
// falsely report "context deadline exceeded". Each check must get its own
// independent budget.
func TestHealthChecksHaveIndependentBudgets(t *testing.T) {
	if healthCheckTimeout <= 2500*time.Millisecond {
		t.Fatalf("healthCheckTimeout=%v too small for this test's timings", healthCheckTimeout)
	}
	s := &Service{
		cfg:    config.Config{TopK: 1, Threshold: 0.25},
		embed:  slowHealthEmbedder{delay: 2500 * time.Millisecond},
		vstore: needsTimeVectorStore{fakeVectorStore: newFakeVectorStore(), need: 1 * time.Second},
		store:  persistence.NewMemoryStore(),
		policy: mustPolicy(t),
		llm:    &countingLLM{},
		queue:  &countingQueue{},
	}

	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s, want 200 (both dependencies are healthy, just slow)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"status":"ok"`) {
		t.Fatalf("body=%s, want overall status ok", rec.Body.String())
	}
}
