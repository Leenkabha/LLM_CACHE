// Package orchestrator implements the central query-flow coordinator:
// embed -> vector search -> hit/miss decision -> LLM on miss -> async cache update.
package orchestrator

import (
	"context"
	"encoding/json"
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
	embed  *embedder.Client
	vstore *vectorstore.Client
	llm    llm.Backend
	store  persistence.Store
	queue  *cachequeue.RedisStreamQueue
	policy *policy.Manager

	mu     sync.Mutex
	hits   int
	misses int
}

func New(cfg config.Config) (*Service, error) {
	store, err := persistence.NewRedisStore(cfg.RedisAddr)
	if err != nil {
		return nil, fmt.Errorf("connect persistence store at %s: %w", cfg.RedisAddr, err)
	}
	queue, err := cachequeue.NewRedisStreamQueue(cfg.RedisAddr)
	if err != nil {
		return nil, fmt.Errorf("connect cache-update queue at %s: %w", cfg.RedisAddr, err)
	}

	svc := &Service{
		cfg:    cfg,
		embed:  embedder.New(cfg.EmbeddingURL),
		vstore: vectorstore.New(cfg.VectorStoreURL),
		llm:    llm.New(cfg.LLMMode, cfg.OpenAIKey, cfg.OpenAIModel),
		store:  store,
		queue:  queue,
		policy: policy.New(cfg.Policy),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := svc.rebuildFromStore(ctx); err != nil {
		return nil, err
	}
	go svc.runCacheWorker(context.Background())

	return svc, nil
}

// Routes registers all orchestrator HTTP endpoints.
func (s *Service) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /query", s.handleQuery)
	mux.HandleFunc("GET /stats", s.handleStats)
	mux.HandleFunc("POST /flush", s.handleFlush)
	mux.HandleFunc("POST /policy", s.handlePolicy)
	mux.HandleFunc("GET /health", s.handleHealth)
	return mux
}

type queryRequest struct {
	Prompt string `json:"prompt"`
}

type queryResponse struct {
	Reply     string  `json:"reply"`
	CacheHit  bool    `json:"cache_hit"`
	Distance  float64 `json:"distance"`
	LatencyMS int64   `json:"latency_ms"`
	Source    string  `json:"source"` // "cache" or "llm"
}

func (s *Service) handleQuery(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	var req queryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Prompt == "" {
		writeError(w, http.StatusBadRequest, "missing or invalid prompt")
		return
	}
	ctx := r.Context()

	vec, err := s.embed.Embed(ctx, req.Prompt)
	if err != nil {
		writeError(w, http.StatusBadGateway, "embedding failed: "+err.Error())
		return
	}

	match, err := s.vstore.Search(ctx, vec, 1, s.cfg.Threshold)
	if err != nil {
		writeError(w, http.StatusBadGateway, "vector search failed: "+err.Error())
		return
	}

	// Cache hit: reply lives in persistence, keyed by the matched entry id.
	if match != nil {
		if entry, ok := s.store.Load(match.ID); ok {
			s.policy.OnHit(match.ID)
			s.recordHit()
			writeJSON(w, queryResponse{
				Reply:     entry.Reply,
				CacheHit:  true,
				Distance:  match.Distance,
				LatencyMS: time.Since(start).Milliseconds(),
				Source:    "cache",
			})
			return
		}
		// Vector matched but reply is gone (inconsistency) -> treat as miss.
	}

	// Cache miss: call the LLM, return immediately, update the cache async.
	reply, err := s.llm.Complete(ctx, req.Prompt)
	if err != nil {
		writeError(w, http.StatusBadGateway, "llm failed: "+err.Error())
		return
	}
	s.recordMiss()
	if err := s.queue.Enqueue(ctx, cachequeue.Job{Prompt: req.Prompt, Reply: reply, Vector: vec}); err != nil {
		log.Printf("enqueue cache update failed: %v", err)
	}

	writeJSON(w, queryResponse{
		Reply:     reply,
		CacheHit:  false,
		Distance:  -1,
		LatencyMS: time.Since(start).Milliseconds(),
		Source:    "llm",
	})
}

func (s *Service) runCacheWorker(ctx context.Context) {
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
		return err
	}
	s.policy.OnInsert(id)
	return s.enforceCapacity(ctx)
}

// rebuildFromStore restores the RAM-only vector index from durable cache
// entries and rebuilds policy-owned metadata after a restart.
func (s *Service) rebuildFromStore(ctx context.Context) error {
	entries, err := s.store.List()
	if err != nil {
		return fmt.Errorf("list cache entries for rebuild: %w", err)
	}

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
			return err
		}
		if err := s.vstore.Delete(ctx, victimID); err != nil {
			return err
		}
		s.policy.OnDelete(victimID)
	}
}

type statsResponse struct {
	Hits    int     `json:"hits"`
	Misses  int     `json:"misses"`
	HitRate float64 `json:"hit_rate"`
	Size    int     `json:"size"`
	Policy  string  `json:"policy"`
}

func (s *Service) handleStats(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	hits, misses := s.hits, s.misses
	s.mu.Unlock()

	total := hits + misses
	var rate float64
	if total > 0 {
		rate = float64(hits) / float64(total)
	}
	size, _ := s.store.Size()

	writeJSON(w, statsResponse{
		Hits:    hits,
		Misses:  misses,
		HitRate: rate,
		Size:    size,
		Policy:  s.policy.Current(),
	})
}

func (s *Service) handleFlush(w http.ResponseWriter, r *http.Request) {
	if err := s.vstore.Flush(r.Context()); err != nil {
		writeError(w, http.StatusBadGateway, "flush failed: "+err.Error())
		return
	}
	_ = s.store.Flush()
	s.policy.Flush()
	s.mu.Lock()
	s.hits, s.misses = 0, 0
	s.mu.Unlock()
	writeJSON(w, map[string]string{"status": "flushed"})
}

type policyRequest struct {
	Policy string `json:"policy"`
}

func (s *Service) handlePolicy(w http.ResponseWriter, r *http.Request) {
	var req policyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}
	if err := s.policy.Set(req.Policy); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]string{"status": "ok", "policy": s.policy.Current()})
}

func (s *Service) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	checks := map[string]string{
		"embedding":   "ok",
		"vectorstore": "ok",
		"redis":       "ok",
	}
	status := "ok"
	if err := s.embed.Health(ctx); err != nil {
		checks["embedding"] = err.Error()
		status = "degraded"
	}
	if err := s.vstore.Health(ctx); err != nil {
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

func (s *Service) recordHit()  { s.mu.Lock(); s.hits++; s.mu.Unlock() }
func (s *Service) recordMiss() { s.mu.Lock(); s.misses++; s.mu.Unlock() }

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
