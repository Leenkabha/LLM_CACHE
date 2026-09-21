package testutil

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"hash/fnv"
	"math"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/leenkabha/llm_cache/internal/cachequeue"
	"github.com/leenkabha/llm_cache/internal/embedder"
	"github.com/leenkabha/llm_cache/internal/vectorstore"
)

// HashEmbedder is the built-in embedder for tests. Its algorithm matches
// sdk/examples/embedder, so a plugin configured with the same dimension and
// model name really does produce the same vector space.
type HashEmbedder struct {
	Dim   int
	Model string
	Calls atomic.Int64
}

func NewHashEmbedder(dim int) *HashEmbedder {
	return &HashEmbedder{Dim: dim, Model: "sdk-hash-embedder"}
}

func (h *HashEmbedder) name() string { return fmt.Sprintf("%s-d%d", h.Model, h.Dim) }

func (h *HashEmbedder) Embed(_ context.Context, text string) ([]float64, error) {
	h.Calls.Add(1)
	v := make([]float64, h.Dim)
	tokens := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	if len(tokens) == 0 {
		tokens = []string{"<empty>"}
	}
	for _, t := range tokens {
		f := fnv.New64a()
		_, _ = f.Write([]byte(t))
		sum := f.Sum64()
		v[int(sum%uint64(h.Dim))] += 1
		if sum&(1<<40) != 0 {
			v[int((sum>>20)%uint64(h.Dim))] -= 0.5
		}
	}
	var n float64
	for _, x := range v {
		n += x * x
	}
	if n == 0 {
		v[0] = 1
		return v, nil
	}
	n = math.Sqrt(n)
	for i := range v {
		v[i] /= n
	}
	return v, nil
}
func (h *HashEmbedder) Health(context.Context) error { return nil }
func (h *HashEmbedder) ModelInfo(context.Context) (embedder.ModelInfo, error) {
	return embedder.ModelInfo{Name: h.name(), Dim: h.Dim}, nil
}

// MemVectorStore is a brute-force cosine vector store for tests.
type MemVectorStore struct {
	Dim  int
	mu   sync.RWMutex
	vecs map[string][]float64
}

func NewMemVectorStore(dim int) *MemVectorStore {
	return &MemVectorStore{Dim: dim, vecs: map[string][]float64{}}
}

func cosDist(a, b []float64) float64 {
	var dot, na, nb float64
	for i := range a {
		dot += a[i] * b[i]
		na += a[i] * a[i]
		nb += b[i] * b[i]
	}
	if na == 0 || nb == 0 {
		return 1
	}
	d := 1 - dot/(math.Sqrt(na)*math.Sqrt(nb))
	if d < 0 && d > -1e-9 {
		d = 0
	}
	return d
}

func (s *MemVectorStore) Search(_ context.Context, v []float64, k int, th float64) ([]vectorstore.Match, error) {
	if len(v) != s.Dim {
		return nil, fmt.Errorf("dimension %d, want %d", len(v), s.Dim)
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	var ms []vectorstore.Match
	for id, x := range s.vecs {
		if d := cosDist(v, x); d <= th {
			ms = append(ms, vectorstore.Match{ID: id, Distance: d})
		}
	}
	sort.Slice(ms, func(i, j int) bool {
		if ms[i].Distance != ms[j].Distance {
			return ms[i].Distance < ms[j].Distance
		}
		return ms[i].ID < ms[j].ID
	})
	if len(ms) > k {
		ms = ms[:k]
	}
	if ms == nil {
		ms = []vectorstore.Match{}
	}
	return ms, nil
}
func (s *MemVectorStore) Upsert(_ context.Context, v []float64) (string, error) {
	if len(v) != s.Dim {
		return "", fmt.Errorf("dimension %d, want %d", len(v), s.Dim)
	}
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	id := hex.EncodeToString(b)
	s.mu.Lock()
	s.vecs[id] = append([]float64(nil), v...)
	s.mu.Unlock()
	return id, nil
}
func (s *MemVectorStore) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.vecs[id]; !ok {
		return fmt.Errorf("vector store returned 404")
	}
	delete(s.vecs, id)
	return nil
}
func (s *MemVectorStore) Size(context.Context) (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.vecs), nil
}
func (s *MemVectorStore) Flush(context.Context) error {
	s.mu.Lock()
	s.vecs = map[string][]float64{}
	s.mu.Unlock()
	return nil
}
func (s *MemVectorStore) Rebuild(_ context.Context, es []vectorstore.RebuildEntry) (int, error) {
	next := map[string][]float64{}
	for _, e := range es {
		if len(e.Vector) != s.Dim {
			return 0, fmt.Errorf("entry %s has dimension %d, want %d", e.ID, len(e.Vector), s.Dim)
		}
		next[e.ID] = append([]float64(nil), e.Vector...)
	}
	s.mu.Lock()
	s.vecs = next
	s.mu.Unlock()
	return len(next), nil
}
func (s *MemVectorStore) Health(context.Context) error { return nil }
func (s *MemVectorStore) Info(context.Context) (vectorstore.Info, error) {
	return vectorstore.Info{Dim: s.Dim, Metric: "cosine"}, nil
}

// MemQueue is an in-memory cachequeue.Queue with acknowledgement-after-success
// and Depth, for tests.
type MemQueue struct {
	mu   sync.Mutex
	jobs []cachequeue.Job
}

func (q *MemQueue) Enqueue(_ context.Context, j cachequeue.Job) error {
	q.mu.Lock()
	q.jobs = append(q.jobs, j)
	q.mu.Unlock()
	return nil
}
func (q *MemQueue) Depth(context.Context) (int, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.jobs), nil
}
func (q *MemQueue) Run(ctx context.Context, h func(context.Context, cachequeue.Job) error) {
	for ctx.Err() == nil {
		q.mu.Lock()
		var j *cachequeue.Job
		if len(q.jobs) > 0 {
			c := q.jobs[0]
			j = &c
		}
		q.mu.Unlock()
		if j == nil {
			time.Sleep(3 * time.Millisecond)
			continue
		}
		if err := h(ctx, *j); err != nil {
			time.Sleep(5 * time.Millisecond)
			continue
		}
		q.mu.Lock()
		q.jobs = q.jobs[1:]
		q.mu.Unlock()
	}
}
