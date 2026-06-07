// Package orchestrator implements the central query-flow coordinator:
// embed -> vector search -> hit/miss decision -> LLM on miss -> async cache update.
package orchestrator

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

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
	policy *policy.Manager

	mu     sync.Mutex
	hits   int
	misses int
}

func New(cfg config.Config) *Service {
	return &Service{
		cfg:    cfg,
		embed:  embedder.New(cfg.EmbeddingURL),
		vstore: vectorstore.New(cfg.VectorStoreURL),
		llm:    llm.New(cfg.LLMMode, cfg.OpenAIKey, cfg.OpenAIModel),
		store:  persistence.NewMemoryStore(),
		policy: policy.New(cfg.Policy),
	}
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
		if reply, ok := s.store.Load(match.ID); ok {
			s.recordHit()
			writeJSON(w, queryResponse{
				Reply:     reply,
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
	go s.updateCache(req.Prompt, vec, reply)

	writeJSON(w, queryResponse{
		Reply:     reply,
		CacheHit:  false,
		Distance:  -1,
		LatencyMS: time.Since(start).Milliseconds(),
		Source:    "llm",
	})
}

// updateCache stores a new <vector, reply> pair. Runs in a goroutine to keep
// it off the request path; week 9 replaces this with a Redis-backed queue.
func (s *Service) updateCache(_ string, vec []float64, reply string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	id, err := s.vstore.Upsert(ctx, vec)
	if err != nil {
		return
	}
	_ = s.store.Save(id, reply)
	// TODO(week 9): enforce capacity via the policy manager (evict on full).
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
	size, _ := s.vstore.Size(r.Context())

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

func (s *Service) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]string{"status": "ok"})
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
