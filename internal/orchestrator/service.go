// Package orchestrator implements the central query-flow coordinator:
// embed -> vector search -> hit/miss decision -> LLM on miss -> async cache update.
package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/leenkabha/llm_cache/internal/cachequeue"
	"github.com/leenkabha/llm_cache/internal/config"
	"github.com/leenkabha/llm_cache/internal/embedder"
	"github.com/leenkabha/llm_cache/internal/llm"
	"github.com/leenkabha/llm_cache/internal/persistence"
	"github.com/leenkabha/llm_cache/internal/policy"
	"github.com/leenkabha/llm_cache/internal/vectorstore"
)

type Service struct {
	cfg    config.Config
	embed  embedder.Embedder
	vstore vectorstore.VectorStore
	llm    llm.Backend
	store  persistence.Store
	queue  cachequeue.Queue
	policy *policy.Manager

	mu             sync.Mutex
	hits           int
	misses         int
	hitLatencySum  time.Duration
	missLatencySum time.Duration
	hitDistanceSum float64
	evictions      int
}

// Dependencies bundles the pluggable adapters the orchestrator runs on.
//
// Every field is an interface (a "port"), so the orchestrator never names a
// concrete implementation. Build the default set from config with
// BuildDependencies, or construct the fields directly -- with alternative
// backends or test doubles -- and pass them to NewWithDependencies.
type Dependencies struct {
	Embedder    embedder.Embedder
	VectorStore vectorstore.VectorStore
	LLM         llm.Backend
	Store       persistence.Store
	Queue       cachequeue.Queue
	Policy      *policy.Manager
}

// BuildDependencies assembles the default adapter set selected by cfg.
//
// This is the single place configuration is mapped to concrete implementations:
// each seam is resolved through its package factory, so adding or swapping a
// backend never touches the orchestrator's request logic.
func BuildDependencies(cfg config.Config) (Dependencies, error) {
	embed, err := embedder.New(cfg)
	if err != nil {
		return Dependencies{}, fmt.Errorf("build embedder: %w", err)
	}
	vstore, err := vectorstore.New(cfg)
	if err != nil {
		return Dependencies{}, fmt.Errorf("build vector store: %w", err)
	}
	backend, err := llm.New(cfg)
	if err != nil {
		return Dependencies{}, fmt.Errorf("build llm backend: %w", err)
	}
	store, err := persistence.New(cfg)
	if err != nil {
		return Dependencies{}, fmt.Errorf("build persistence store: %w", err)
	}
	queue, err := cachequeue.New(cfg)
	if err != nil {
		return Dependencies{}, fmt.Errorf("build cache-update queue: %w", err)
	}
	pol, err := policy.New(cfg)
	if err != nil {
		return Dependencies{}, fmt.Errorf("build policy: %w", err)
	}

	return Dependencies{
		Embedder:    embed,
		VectorStore: vstore,
		LLM:         backend,
		Store:       store,
		Queue:       queue,
		Policy:      pol,
	}, nil
}

// New builds the orchestrator with the default config-selected adapters.
func New(cfg config.Config) (*Service, error) {
	if cfg.TopK < 1 {
		return nil, fmt.Errorf("TopK must be at least 1")
	}
	deps, err := BuildDependencies(cfg)
	if err != nil {
		return nil, err
	}
	return NewWithDependencies(cfg, deps)
}

// NewWithDependencies builds the orchestrator from an explicit adapter set,
// then restores state and starts the async cache worker. New is the
// config-driven entry point; this exists for tests and custom wiring.
func NewWithDependencies(cfg config.Config, deps Dependencies) (*Service, error) {
	if cfg.TopK < 1 {
		return nil, fmt.Errorf("TopK must be at least 1")
	}
	log.Printf("orchestrator startup: policy=%s capacity=%d persistence=%s queue=%s embedding=%s vectorstore=%s llm_mode=%s",
		cfg.Policy, cfg.Capacity, cfg.PersistenceBackend, cfg.QueueBackend, cfg.EmbeddingBackend, cfg.VectorStoreBackend, cfg.LLMMode)

	svc := &Service{
		cfg:    cfg,
		embed:  deps.Embedder,
		vstore: deps.VectorStore,
		llm:    deps.LLM,
		store:  deps.Store,
		queue:  deps.Queue,
		policy: deps.Policy,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := svc.rebuildFromStore(ctx); err != nil {
		return nil, err
	}
	log.Printf("cache worker starting: queue_backend=%s", cfg.QueueBackend)
	go svc.runCacheWorker(context.Background())

	return svc, nil
}

// Routes registers all orchestrator HTTP endpoints.
func (s *Service) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleUI)
	mux.HandleFunc("POST /query", s.handleQuery)
	mux.HandleFunc("GET /stats", s.handleStats)
	mux.HandleFunc("POST /flush", s.handleFlush)
	mux.HandleFunc("POST /policy", s.handlePolicy)
	mux.HandleFunc("GET /health", s.handleHealth)
	return newGuard(mux, s.cfg.AdminToken, s.cfg.RateLimitPerMin, s.cfg.TrustProxy)
}

type queryRequest struct {
	Prompt string `json:"prompt"`
}

type queryResult struct {
	ID       string  `json:"id"`
	Reply    string  `json:"reply"`
	Distance float64 `json:"distance"`
}

type queryResponse struct {
	Results   []queryResult `json:"results"`
	Reply     string        `json:"reply"`
	CacheHit  bool          `json:"cache_hit"`
	Distance  float64       `json:"distance"`
	LatencyMS int64         `json:"latency_ms"`
	Source    string        `json:"source"` // "cache" or "llm"
}

// distanceEpsilon bounds the float32 rounding noise a vector store's
// similarity metric can introduce for a near-identical vector (observed:
// exactly one float32 ULP at magnitude 1.0, ~1.19e-7, from FAISS's cosine
// metric). It is far smaller than any real similarity difference -- the
// default SIMILARITY_THRESHOLD is 0.25 -- so it can never mask one.
const distanceEpsilon = 1e-6

// normalizeDistance clamps a tiny negative floating-point artifact to 0
// without touching the -1 MISS sentinel (set independently, well outside this
// band) or any other value outside the noise band, so an unexpected large
// negative distance is never silently hidden.
//
// This is the single point every accepted vector-store match's distance
// passes through, so the normalized value propagates consistently to the
// top-level /query distance, results[].distance, hit logs, and
// avg_hit_distance -- regardless of which VectorStoreBackend produced it.
func normalizeDistance(d float64) float64 {
	if d < 0 && d > -distanceEpsilon {
		return 0
	}
	return d
}

// maxQueryBodyBytes caps a /query request so one caller cannot push huge
// prompts through to the LLM and burn its quota.
const maxQueryBodyBytes = 16 << 10

func (s *Service) handleQuery(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	var req queryRequest
	r.Body = http.MaxBytesReader(w, r.Body, maxQueryBodyBytes)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Prompt == "" {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			log.Printf("query rejected: body_too_large remote=%s", r.RemoteAddr)
			writeError(w, http.StatusRequestEntityTooLarge, "prompt too large")
			return
		}
		log.Printf("query rejected: missing_or_invalid_prompt remote=%s", r.RemoteAddr)
		writeError(w, http.StatusBadRequest, "missing or invalid prompt")
		return
	}
	ctx := r.Context()

	vec, err := s.embed.Embed(ctx, req.Prompt)
	if err != nil {
		log.Printf("query embedding failed: remote=%s err=%v", r.RemoteAddr, err)
		writeError(w, http.StatusBadGateway, "embedding failed: "+err.Error())
		return
	}

	matches, err := s.vstore.Search(ctx, vec, s.cfg.TopK, s.cfg.Threshold)
	if err != nil {
		log.Printf("query vector search failed: remote=%s err=%v", r.RemoteAddr, err)
		writeError(w, http.StatusBadGateway, "vector search failed: "+err.Error())
		return
	}

	// Preserve the vector store's best-to-worst order while skipping stale IDs.
	results := make([]queryResult, 0, len(matches))
	for _, match := range matches {
		entry, ok := s.store.Load(match.ID)
		if !ok {
			log.Printf("cache inconsistency: vector_match_id=%s missing_from_store=true", match.ID)
			continue
		}
		results = append(results, queryResult{ID: match.ID, Reply: entry.Reply, Distance: normalizeDistance(match.Distance)})
		s.policy.OnHit(match.ID)
	}
	if len(results) > 0 {
		elapsed := time.Since(start)
		best := results[0]
		s.recordHit(elapsed, best.Distance)
		log.Printf("cache hit: id=%s distance=%.6f matches=%d policy=%s latency_ms=%d",
			best.ID, best.Distance, len(results), s.policy.Current(), elapsed.Milliseconds())
		writeJSON(w, queryResponse{
			Reply:     best.Reply,
			CacheHit:  true,
			Distance:  best.Distance,
			Results:   results,
			LatencyMS: elapsed.Milliseconds(),
			Source:    "cache",
		})
		return
	}

	// Cache miss: call the LLM, return immediately, update the cache async.
	reply, err := s.llm.Complete(ctx, req.Prompt)
	if err != nil {
		log.Printf("query llm failed: remote=%s err=%v", r.RemoteAddr, err)
		writeError(w, http.StatusBadGateway, "llm failed: "+err.Error())
		return
	}
	elapsed := time.Since(start)
	s.recordMiss(elapsed)
	if err := s.queue.Enqueue(ctx, cachequeue.Job{Prompt: req.Prompt, Reply: reply, Vector: vec}); err != nil {
		log.Printf("enqueue cache update failed: %v", err)
	} else {
		log.Printf("cache miss: queued cache update latency_ms=%d", elapsed.Milliseconds())
	}

	writeJSON(w, queryResponse{
		Reply:     reply,
		CacheHit:  false,
		Results:   []queryResult{},
		Distance:  -1,
		LatencyMS: elapsed.Milliseconds(),
		Source:    "llm",
	})
}

func (s *Service) runCacheWorker(ctx context.Context) {
	log.Printf("cache worker running")
	s.queue.Run(ctx, s.handleCacheUpdateJob)
}

func (s *Service) handleCacheUpdateJob(ctx context.Context, job cachequeue.Job) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return s.updateCache(ctx, job.Prompt, job.Vector, job.Reply)
}

// updateCache stores a new <vector, reply> pair from the Redis Streams worker.
func (s *Service) updateCache(ctx context.Context, prompt string, vec []float64, reply string) error {
	id, err := s.vstore.Upsert(ctx, vec)
	if err != nil {
		log.Printf("cache update failed: vector_upsert err=%v", err)
		return err
	}
	if err := s.store.Save(persistence.Entry{
		ID:        id,
		Prompt:    prompt,
		Reply:     reply,
		Vector:    vec,
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		_ = s.vstore.Delete(ctx, id)
		log.Printf("cache update failed: store_save id=%s err=%v", id, err)
		return err
	}
	s.policy.OnInsert(id)
	if err := s.enforceCapacity(ctx); err != nil {
		log.Printf("cache update failed: enforce_capacity id=%s err=%v", id, err)
		return err
	}
	log.Printf("cache update saved: id=%s policy=%s", id, s.policy.Current())
	return nil
}

// rebuildFromStore restores the RAM-only vector index from durable cache
// entries and rebuilds policy-owned metadata after a restart.
func (s *Service) rebuildFromStore(ctx context.Context) error {
	entries, err := s.store.List()
	if err != nil {
		return fmt.Errorf("list cache entries for rebuild: %w", err)
	}
	log.Printf("cache rebuild starting: entries=%d", len(entries))

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].CreatedAt.Before(entries[j].CreatedAt)
	})

	rebuildEntries := make([]vectorstore.RebuildEntry, 0, len(entries))
	for _, entry := range entries {
		rebuildEntries = append(rebuildEntries, vectorstore.RebuildEntry{
			ID:     entry.ID,
			Vector: entry.Vector,
		})
		s.policy.OnInsert(entry.ID)
	}

	if err := s.rebuildVectorStoreWithRetry(ctx, rebuildEntries); err != nil {
		return fmt.Errorf("rebuild vector store from cache: %w", err)
	}
	log.Printf("cache rebuild completed: entries=%d", len(entries))
	return nil
}

func (s *Service) rebuildVectorStoreWithRetry(ctx context.Context, entries []vectorstore.RebuildEntry) error {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	var lastErr error
	for {
		if _, err := s.vstore.Rebuild(ctx, entries); err == nil {
			return nil
		} else {
			lastErr = err
		}

		select {
		case <-ctx.Done():
			if lastErr != nil {
				return lastErr
			}
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// enforceCapacity keeps the durable cache at or below CACHE_CAPACITY.
//
// The orchestrator coordinates eviction, but the active policy owns victim
// selection. This keeps persistence entries policy-agnostic while still making
// capacity enforcement pluggable.
func (s *Service) enforceCapacity(ctx context.Context) error {
	if s.cfg.Capacity <= 0 {
		return nil
	}

	for {
		size, err := s.store.Size()
		if err != nil {
			return err
		}
		if size <= s.cfg.Capacity {
			return nil
		}

		victimID, ok := s.policy.Victim()
		if !ok {
			return fmt.Errorf("cache size %d exceeds capacity %d, but policy %q has no victim", size, s.cfg.Capacity, s.policy.Current())
		}

		if err := s.store.Delete(victimID); err != nil {
			log.Printf("cache eviction failed: store_delete id=%s err=%v", victimID, err)
			return err
		}
		if err := s.vstore.Delete(ctx, victimID); err != nil {
			log.Printf("cache eviction failed: vector_delete id=%s err=%v", victimID, err)
			return err
		}
		s.policy.OnDelete(victimID)
		s.recordEviction()
		log.Printf("cache eviction completed: id=%s policy=%s size_before=%d capacity=%d", victimID, s.policy.Current(), size, s.cfg.Capacity)
	}
}

type statsResponse struct {
	Requests         int     `json:"requests"`
	Hits             int     `json:"hits"`
	Misses           int     `json:"misses"`
	HitRate          float64 `json:"hit_rate"`
	AvgHitLatencyMS  float64 `json:"avg_hit_latency_ms"`
	AvgMissLatencyMS float64 `json:"avg_miss_latency_ms"`
	AvgHitDistance   float64 `json:"avg_hit_distance"`
	Evictions        int     `json:"evictions"`
	Size             int     `json:"size"`
	Policy           string  `json:"policy"`
}

func (s *Service) handleStats(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	hits, misses := s.hits, s.misses
	hitLatencySum, missLatencySum := s.hitLatencySum, s.missLatencySum
	hitDistanceSum := s.hitDistanceSum
	evictions := s.evictions
	s.mu.Unlock()

	requests := hits + misses
	var rate float64
	if requests > 0 {
		rate = float64(hits) / float64(requests)
	}
	var avgHitLatency, avgMissLatency, avgHitDistance float64
	if hits > 0 {
		avgHitLatency = float64(hitLatencySum) / float64(hits) / float64(time.Millisecond)
		avgHitDistance = hitDistanceSum / float64(hits)
	}
	if misses > 0 {
		avgMissLatency = float64(missLatencySum) / float64(misses) / float64(time.Millisecond)
	}
	size, _ := s.store.Size()
	log.Printf("stats requested: requests=%d hits=%d misses=%d evictions=%d size=%d policy=%s",
		requests, hits, misses, evictions, size, s.policy.Current())

	writeJSON(w, statsResponse{
		Requests:         requests,
		Hits:             hits,
		Misses:           misses,
		HitRate:          rate,
		AvgHitLatencyMS:  avgHitLatency,
		AvgMissLatencyMS: avgMissLatency,
		AvgHitDistance:   avgHitDistance,
		Evictions:        evictions,
		Size:             size,
		Policy:           s.policy.Current(),
	})
}

func (s *Service) handleFlush(w http.ResponseWriter, r *http.Request) {
	log.Printf("flush requested")
	if err := s.vstore.Flush(r.Context()); err != nil {
		log.Printf("flush failed: vectorstore err=%v", err)
		writeError(w, http.StatusBadGateway, "flush failed: "+err.Error())
		return
	}
	if err := s.store.Flush(); err != nil {
		log.Printf("flush warning: store err=%v", err)
	}
	s.policy.Flush()
	s.mu.Lock()
	s.hits, s.misses = 0, 0
	s.hitLatencySum, s.missLatencySum = 0, 0
	s.hitDistanceSum = 0
	s.evictions = 0
	s.mu.Unlock()
	log.Printf("flush completed")
	writeJSON(w, map[string]string{"status": "flushed"})
}

type policyRequest struct {
	Policy string `json:"policy"`
}

func (s *Service) handlePolicy(w http.ResponseWriter, r *http.Request) {
	var req policyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		log.Printf("policy change rejected: invalid_body err=%v", err)
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}
	if err := s.policy.Set(req.Policy); err != nil {
		log.Printf("policy change rejected: requested=%s err=%v", req.Policy, err)
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	log.Printf("policy change accepted: policy=%s", s.policy.Current())
	writeJSON(w, map[string]string{"status": "ok", "policy": s.policy.Current()})
}

// healthCheckTimeout bounds each dependency check independently. Checks run
// sequentially and must not share one deadline: if they did, a slow or down
// dependency checked first could exhaust the whole budget and make later,
// perfectly healthy dependencies falsely report "context deadline exceeded".
const healthCheckTimeout = 3 * time.Second

func (s *Service) handleHealth(w http.ResponseWriter, r *http.Request) {
	checks := map[string]string{
		"embedding":   "ok",
		"vectorstore": "ok",
		"redis":       "ok",
	}
	status := "ok"

	embedCtx, cancel := context.WithTimeout(r.Context(), healthCheckTimeout)
	defer cancel()
	if err := s.embed.Health(embedCtx); err != nil {
		checks["embedding"] = err.Error()
		status = "degraded"
	}

	vstoreCtx, cancel := context.WithTimeout(r.Context(), healthCheckTimeout)
	defer cancel()
	if err := s.vstore.Health(vstoreCtx); err != nil {
		checks["vectorstore"] = err.Error()
		status = "degraded"
	}

	if err := s.store.Health(); err != nil {
		checks["redis"] = err.Error()
		status = "degraded"
	}

	if status != "ok" {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	writeJSON(w, map[string]any{"status": status, "checks": checks})
}

func (s *Service) recordHit(latency time.Duration, distance float64) {
	s.mu.Lock()
	s.hits++
	s.hitLatencySum += latency
	s.hitDistanceSum += distance
	s.mu.Unlock()
}

func (s *Service) recordMiss(latency time.Duration) {
	s.mu.Lock()
	s.misses++
	s.missLatencySum += latency
	s.mu.Unlock()
}

func (s *Service) recordEviction() {
	s.mu.Lock()
	s.evictions++
	s.mu.Unlock()
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
