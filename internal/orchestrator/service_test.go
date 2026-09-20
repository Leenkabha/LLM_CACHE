package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/leenkabha/llm_cache/internal/cachequeue"
	"github.com/leenkabha/llm_cache/internal/config"
	"github.com/leenkabha/llm_cache/internal/embedder"
	"github.com/leenkabha/llm_cache/internal/persistence"
	"github.com/leenkabha/llm_cache/internal/policy"
	"github.com/leenkabha/llm_cache/internal/vectorstore"
)

type fixedEmbedder struct{ embedder.Embedder }

func (fixedEmbedder) Embed(context.Context, string) ([]float64, error) { return []float64{1, 0}, nil }

type countingLLM struct{ calls int }

func (l *countingLLM) Complete(context.Context, string) (string, error) {
	l.calls++
	return "fresh reply", nil
}

type countingQueue struct {
	cachequeue.Queue
	jobs []cachequeue.Job
}

func (q *countingQueue) Enqueue(_ context.Context, j cachequeue.Job) error {
	q.jobs = append(q.jobs, j)
	return nil
}

type fixedSearch struct {
	vectorstore.VectorStore
	matches   []vectorstore.Match
	err       error
	k         int
	threshold float64
}

func (v *fixedSearch) Search(_ context.Context, _ []float64, k int, threshold float64) ([]vectorstore.Match, error) {
	v.k = k
	v.threshold = threshold
	return v.matches, v.err
}

func queryFixture(t *testing.T, v vectorstore.VectorStore, k int) (*Service, *countingLLM, *countingQueue) {
	t.Helper()
	store := persistence.NewMemoryStore()
	pol, err := policy.NewManager("lfu")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b", "c"} {
		if err := store.Save(persistence.Entry{ID: id, Reply: "reply " + id}); err != nil {
			t.Fatal(err)
		}
		pol.OnInsert(id)
	}
	backend := &countingLLM{}
	queue := &countingQueue{}
	return &Service{cfg: config.Config{TopK: k, Threshold: 0.25}, embed: fixedEmbedder{}, vstore: v, store: store, policy: pol, llm: backend, queue: queue}, backend, queue
}

func performQuery(t *testing.T, s *Service) (queryResponse, int) {
	t.Helper()
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/query", strings.NewReader(`{"prompt":"test"}`)))
	var out queryResponse
	if rec.Code == 200 {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if out.Results == nil {
			t.Fatalf("results must be an array: %s", rec.Body.String())
		}
	}
	return out, rec.Code
}

func TestNormalizeDistance(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   float64
		want float64
	}{
		{"exact zero unchanged", 0, 0},
		{"actual observed FAISS artifact clamped", -1.1920928955078125e-7, 0},
		{"value just inside epsilon clamped", -9.9e-7, 0},
		{"epsilon boundary unchanged (not < -epsilon)", -1e-6, -1e-6},
		{"normal positive distance unchanged", 0.0381, 0.0381},
		{"miss sentinel unchanged", -1, -1},
		{"larger negative value unchanged, not silently hidden", -0.5, -0.5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeDistance(tc.in); got != tc.want {
				t.Fatalf("normalizeDistance(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestQueryNormalizesTinyNegativeHitDistance is a regression test for a real
// FAISS float32 artifact observed during E2E testing: a near-identical vector
// can score a distance of -1.1920928955078125e-7 (exactly one float32 ULP)
// instead of exactly 0. This must be normalized consistently everywhere a hit
// distance is exposed: the top-level response, results[], and avg_hit_distance.
func TestQueryNormalizesTinyNegativeHitDistance(t *testing.T) {
	const artifact = -1.1920928955078125e-7
	s, _, _ := queryFixture(t, &fixedSearch{matches: []vectorstore.Match{{ID: "a", Distance: artifact}}}, 1)
	out, status := performQuery(t, s)
	if status != 200 {
		t.Fatalf("status=%d", status)
	}
	if !out.CacheHit || out.Source != "cache" {
		t.Fatalf("expected a hit, got response=%+v", out)
	}
	if out.Distance != 0 {
		t.Fatalf("top-level distance = %v, want 0", out.Distance)
	}
	if len(out.Results) != 1 || out.Results[0].Distance != 0 {
		t.Fatalf("results[0].distance = %+v, want 0", out.Results)
	}

	stats := fetchStats(t, s)
	if stats.AvgHitDistance != 0 {
		t.Fatalf("avg_hit_distance = %v, want 0", stats.AvgHitDistance)
	}
}

func TestQueryMatches(t *testing.T) {
	for _, tc := range []struct {
		name    string
		k       int
		matches []vectorstore.Match
		want    []string
	}{
		{"multiple", 3, []vectorstore.Match{{ID: "a", Distance: 0.02}, {ID: "b", Distance: 0.1}, {ID: "c", Distance: 0.25}}, []string{"a", "b", "c"}},
		{"fewer than k", 3, []vectorstore.Match{{ID: "b", Distance: 0.1}}, []string{"b"}},
		{"k one", 1, []vectorstore.Match{{ID: "a", Distance: 0.02}}, []string{"a"}},
		{"missing best", 3, []vectorstore.Match{{ID: "gone", Distance: 0.01}, {ID: "b", Distance: 0.1}}, []string{"b"}},
		{"missing middle", 3, []vectorstore.Match{{ID: "a", Distance: 0.02}, {ID: "gone", Distance: 0.04}, {ID: "c", Distance: 0.25}}, []string{"a", "c"}},
		{"all missing", 3, []vectorstore.Match{{ID: "gone", Distance: 0.01}}, nil},
		{"no matches", 3, nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := &fixedSearch{matches: tc.matches}
			s, l, q := queryFixture(t, v, tc.k)
			out, status := performQuery(t, s)
			if status != 200 || v.k != tc.k || v.threshold != 0.25 {
				t.Fatalf("status=%d request=%+v", status, v)
			}
			if len(out.Results) != len(tc.want) {
				t.Fatalf("results=%+v", out.Results)
			}
			for i, id := range tc.want {
				if out.Results[i].ID != id || out.Results[i].Reply != "reply "+id {
					t.Fatalf("result=%+v", out.Results[i])
				}
				if i > 0 && out.Results[i].Distance < out.Results[i-1].Distance {
					t.Fatal("wrong order")
				}
			}
			if len(tc.want) > 0 {
				if !out.CacheHit || out.Source != "cache" || out.Reply != out.Results[0].Reply || out.Distance != out.Results[0].Distance || s.hits != 1 || s.misses != 0 || l.calls != 0 || len(q.jobs) != 0 {
					t.Fatalf("hit response=%+v hits=%d misses=%d llm=%d", out, s.hits, s.misses, l.calls)
				}
			} else {
				if out.CacheHit || out.Source != "llm" || out.Reply != "fresh reply" || out.Distance != -1 || s.hits != 0 || s.misses != 1 || l.calls != 1 || len(q.jobs) != 1 {
					t.Fatalf("miss response=%+v", out)
				}
			}
		})
	}
}

func TestQuerySearchError(t *testing.T) {
	s, l, q := queryFixture(t, &fixedSearch{err: errors.New("offline")}, 3)
	_, status := performQuery(t, s)
	if status != 502 || l.calls != 0 || len(q.jobs) != 0 {
		t.Fatalf("status=%d llm=%d", status, l.calls)
	}
	if s.hits != 0 || s.misses != 0 {
		t.Fatalf("failed query (vector search error) must not record hit/miss stats: hits=%d misses=%d", s.hits, s.misses)
	}
}

func TestInvalidPromptDoesNotRecordStats(t *testing.T) {
	s, _, _ := queryFixture(t, &fixedSearch{}, 1)
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/query", strings.NewReader(`{}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400", rec.Code)
	}
	if s.hits != 0 || s.misses != 0 {
		t.Fatalf("rejected request (missing prompt) must not record hit/miss stats: hits=%d misses=%d", s.hits, s.misses)
	}
}

func TestOversizedPromptRejectedBeforeLLM(t *testing.T) {
	s, l, q := queryFixture(t, &fixedSearch{}, 1)
	body := `{"prompt":"` + strings.Repeat("x", maxQueryBodyBytes) + `"}`
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/query", strings.NewReader(body)))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status=%d, want 413", rec.Code)
	}
	if l.calls != 0 || len(q.jobs) != 0 || s.hits != 0 || s.misses != 0 {
		t.Fatalf("oversized prompt must not reach the LLM or stats: llm=%d jobs=%d hits=%d misses=%d", l.calls, len(q.jobs), s.hits, s.misses)
	}
}

// sleepEmbedder and sleepLLM add a deterministic delay so latency-recording
// tests don't depend on the ambient speed of instant test doubles, which can
// round to a zero duration and make an assertion on "latency was recorded"
// flaky.
type sleepEmbedder struct {
	embedder.Embedder
	d time.Duration
}

func (e sleepEmbedder) Embed(context.Context, string) ([]float64, error) {
	time.Sleep(e.d)
	return []float64{1, 0}, nil
}

type sleepLLM struct{ d time.Duration }

func (l *sleepLLM) Complete(context.Context, string) (string, error) {
	time.Sleep(l.d)
	return "fresh reply", nil
}

func fetchStats(t *testing.T, s *Service) statsResponse {
	t.Helper()
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/stats", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/stats status=%d", rec.Code)
	}
	var stats statsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &stats); err != nil {
		t.Fatalf("decode /stats: %v", err)
	}
	return stats
}

// TestStatsTracksRequestsAndLatencyByOutcome verifies /stats reports requests
// as the sum of hits and misses, and that hit/miss latency are accumulated
// independently -- a cache hit must never contribute to avg_miss_latency_ms
// and vice versa.
func TestStatsTracksRequestsAndLatencyByOutcome(t *testing.T) {
	const delay = 5 * time.Millisecond

	hitSvc, _, _ := queryFixture(t, &fixedSearch{matches: []vectorstore.Match{{ID: "a", Distance: 0.02}}}, 1)
	hitSvc.embed = sleepEmbedder{d: delay}
	if _, status := performQuery(t, hitSvc); status != 200 {
		t.Fatalf("hit query status=%d", status)
	}
	hitStats := fetchStats(t, hitSvc)
	if hitStats.Requests != 1 || hitStats.Hits != 1 || hitStats.Misses != 0 || hitStats.HitRate != 1 {
		t.Fatalf("hit stats=%+v", hitStats)
	}
	if hitStats.AvgHitLatencyMS < float64(delay/time.Millisecond) {
		t.Fatalf("avg_hit_latency_ms=%v, want at least %v (the injected delay)", hitStats.AvgHitLatencyMS, delay)
	}
	if hitStats.AvgMissLatencyMS != 0 {
		t.Fatalf("avg_miss_latency_ms=%v, want 0 (no miss occurred)", hitStats.AvgMissLatencyMS)
	}
	if hitStats.AvgHitDistance != 0.02 {
		t.Fatalf("avg_hit_distance=%v, want 0.02 (the single hit's distance)", hitStats.AvgHitDistance)
	}

	missSvc, _, _ := queryFixture(t, &fixedSearch{}, 1)
	missSvc.llm = &sleepLLM{d: delay}
	if _, status := performQuery(t, missSvc); status != 200 {
		t.Fatalf("miss query status=%d", status)
	}
	missStats := fetchStats(t, missSvc)
	if missStats.Requests != 1 || missStats.Hits != 0 || missStats.Misses != 1 || missStats.HitRate != 0 {
		t.Fatalf("miss stats=%+v", missStats)
	}
	if missStats.AvgMissLatencyMS < float64(delay/time.Millisecond) {
		t.Fatalf("avg_miss_latency_ms=%v, want at least %v (the injected delay)", missStats.AvgMissLatencyMS, delay)
	}
	if missStats.AvgHitLatencyMS != 0 {
		t.Fatalf("avg_hit_latency_ms=%v, want 0 (no hit occurred)", missStats.AvgHitLatencyMS)
	}
	if missStats.AvgHitDistance != 0 {
		t.Fatalf("avg_hit_distance=%v, want 0 (no hit occurred)", missStats.AvgHitDistance)
	}
}

// TestStatsAvgHitDistanceUsesBestMatchOnly verifies that a Top-K hit query
// returning multiple results still contributes exactly one distance value to
// avg_hit_distance -- the best (first, lowest-distance) match -- not an
// average or sum across all K returned results.
func TestStatsAvgHitDistanceUsesBestMatchOnly(t *testing.T) {
	v := &fixedSearch{matches: []vectorstore.Match{
		{ID: "a", Distance: 0.02},
		{ID: "b", Distance: 0.10},
		{ID: "c", Distance: 0.25},
	}}
	s, _, _ := queryFixture(t, v, 3)
	out, status := performQuery(t, s)
	if status != 200 || len(out.Results) != 3 {
		t.Fatalf("status=%d results=%+v", status, out.Results)
	}

	stats := fetchStats(t, s)
	if stats.Hits != 1 {
		t.Fatalf("hits=%d, want 1 (one hit per query regardless of Top-K result count)", stats.Hits)
	}
	if stats.AvgHitDistance != 0.02 {
		t.Fatalf("avg_hit_distance=%v, want 0.02 (best match only, not averaged across all %d results)", stats.AvgHitDistance, len(out.Results))
	}

	// A second Top-K hit with a different best distance and a different
	// number of runner-up results must average with the first hit's best
	// distance only -- proving no runner-up distance from either query ever
	// entered the sum.
	v.matches = []vectorstore.Match{
		{ID: "b", Distance: 0.08},
		{ID: "c", Distance: 0.20},
	}
	out2, status2 := performQuery(t, s)
	if status2 != 200 || len(out2.Results) != 2 {
		t.Fatalf("status=%d results=%+v", status2, out2.Results)
	}
	stats2 := fetchStats(t, s)
	if stats2.Hits != 2 {
		t.Fatalf("hits=%d, want 2", stats2.Hits)
	}
	if want := (0.02 + 0.08) / 2; stats2.AvgHitDistance != want {
		t.Fatalf("avg_hit_distance=%v, want %v (average of the two best distances 0.02 and 0.08 only)", stats2.AvgHitDistance, want)
	}
}

// TestFlushResetsAllMetrics documents that /flush clears every counter this
// change adds, not just the pre-existing hits/misses. It uses fakeVectorStore
// (from consistency_test.go) rather than the query-only fixedSearch stub
// because handleFlush also calls vstore.Flush, which fixedSearch's embedded
// nil vectorstore.VectorStore does not implement.
func TestFlushResetsAllMetrics(t *testing.T) {
	vstore := newFakeVectorStore()
	store := persistence.NewMemoryStore()
	pol, err := policy.NewManager("lru")
	if err != nil {
		t.Fatal(err)
	}
	s := newConsistencyService(t, vstore, store, pol, 0)

	if err := s.updateCache(context.Background(), "cached prompt", []float64{1, 0}, "cached reply"); err != nil {
		t.Fatal(err)
	}
	if _, status := performQuery(t, s); status != 200 {
		t.Fatalf("query status=%d", status)
	}
	s.recordEviction()

	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/flush", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/flush status=%d", rec.Code)
	}

	stats := fetchStats(t, s)
	if stats.Requests != 0 || stats.Hits != 0 || stats.Misses != 0 || stats.HitRate != 0 ||
		stats.AvgHitLatencyMS != 0 || stats.AvgMissLatencyMS != 0 || stats.AvgHitDistance != 0 || stats.Evictions != 0 {
		t.Fatalf("stats after flush not fully reset: %+v", stats)
	}
}

func TestConstructorsRejectInvalidTopK(t *testing.T) {
	for _, k := range []int{0, -1} {
		cfg := config.Config{TopK: k}
		if _, err := New(cfg); err == nil {
			t.Fatalf("New accepted %d", k)
		}
		if _, err := NewWithDependencies(cfg, Dependencies{}); err == nil {
			t.Fatalf("NewWithDependencies accepted %d", k)
		}
	}
}

// Run against a dedicated, empty Python service via scripts/test_topk_integration.py.
func TestQueryPythonIntegration(t *testing.T) {
	url := os.Getenv("TOPK_TEST_VECTORSTORE_URL")
	if url == "" {
		t.Skip("set TOPK_TEST_VECTORSTORE_URL to a dedicated test service")
	}
	v := vectorstore.NewHTTP(url)
	// Unit-length vectors with exactly representable cosine distances 0, .125, .25, and 1.
	entries := []vectorstore.RebuildEntry{
		{ID: "outside", Vector: []float64{0, 1}},
		{ID: "c", Vector: []float64{0.75, 0.6614378277661477}},
		{ID: "a", Vector: []float64{1, 0}},
		{ID: "b", Vector: []float64{0.875, 0.4841229182759271}},
	}
	if _, err := v.Rebuild(context.Background(), entries); err != nil {
		t.Fatal(err)
	}
	for _, k := range []int{1, 2, 3, 10} {
		s, l, _ := queryFixture(t, v, k)
		out, status := performQuery(t, s)
		want := k
		if want > 3 {
			want = 3
		}
		if status != 200 || len(out.Results) != want || l.calls != 0 {
			t.Fatalf("k=%d status=%d response=%+v", k, status, out)
		}
		for i, result := range out.Results {
			if result.ID != []string{"a", "b", "c"}[i] || result.Distance != []float64{0, 0.125, 0.25}[i] {
				t.Fatalf("unexpected result %+v", result)
			}
		}
	}
	// Missing cached replies do not discard later valid matches or trigger an LLM call.
	s, l, q := queryFixture(t, v, 3)
	if err := s.store.Delete("a"); err != nil {
		t.Fatal(err)
	}
	out, status := performQuery(t, s)
	if status != 200 || len(out.Results) != 2 || out.Results[0].ID != "b" || out.Reply != "reply b" || l.calls != 0 || len(q.jobs) != 0 {
		t.Fatalf("stale match response=%+v", out)
	}
	for _, id := range []string{"b", "c"} {
		if err := s.store.Delete(id); err != nil {
			t.Fatal(err)
		}
	}
	out, status = performQuery(t, s)
	if status != 200 || out.CacheHit || len(out.Results) != 0 || l.calls != 1 || len(q.jobs) != 1 {
		t.Fatalf("all stale response=%+v", out)
	}
	// A real vector search with no accepted matches falls back to the LLM.
	s, l, q = queryFixture(t, v, 3)
	s.cfg.Threshold = -1
	out, status = performQuery(t, s)
	if status != 200 || out.CacheHit || len(out.Results) != 0 || l.calls != 1 || len(q.jobs) != 1 {
		t.Fatalf("threshold miss response=%+v", out)
	}

}

func TestQueryUpdatesPolicyForEveryResult(t *testing.T) {
	s, _, _ := queryFixture(t, &fixedSearch{matches: []vectorstore.Match{{ID: "a", Distance: 0.02}, {ID: "b", Distance: 0.1}}}, 3)
	_, status := performQuery(t, s)
	if status != 200 {
		t.Fatalf("status=%d", status)
	}
	victim, ok := s.policy.Victim()
	if !ok || victim != "c" {
		t.Fatalf("LFU victim=%q, want unused entry c", victim)
	}
}
