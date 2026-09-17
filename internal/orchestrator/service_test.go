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
