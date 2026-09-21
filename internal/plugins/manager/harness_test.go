package manager_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/leenkabha/llm_cache/internal/config"
	"github.com/leenkabha/llm_cache/internal/orchestrator"
	"github.com/leenkabha/llm_cache/internal/persistence"
	"github.com/leenkabha/llm_cache/internal/plugins"
	"github.com/leenkabha/llm_cache/internal/plugins/manager"
	"github.com/leenkabha/llm_cache/internal/plugins/registry"
	"github.com/leenkabha/llm_cache/internal/plugins/secrets"
	"github.com/leenkabha/llm_cache/internal/plugins/testutil"
	"github.com/leenkabha/llm_cache/internal/policy"
)

type countingLLM struct{ calls atomic.Int64 }

func (l *countingLLM) Complete(context.Context, string) (string, error) {
	n := l.calls.Add(1)
	return "builtin-reply-" + string(rune('0'+n%10)), nil
}

type harness struct {
	t     *testing.T
	m     *manager.Manager
	reg   *registry.Memory
	slots *manager.Slots
	svc   *orchestrator.Service
	srv   *httptest.Server
	key   string

	llm   *countingLLM
	emb   *testutil.HashEmbedder
	vs    *testutil.MemVectorStore
	store *persistence.MemoryStore
	queue *testutil.MemQueue
}

func newKey() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.StdEncoding.EncodeToString(b)
}

type opts struct {
	dim   int
	cfg   func(*manager.Config)
	ctl   manager.Controller
	key   string
	reg   *registry.Memory
	store *persistence.MemoryStore // reuse persisted entries across a "restart"
}

func newHarness(t *testing.T, o opts) *harness {
	t.Helper()
	if o.dim == 0 {
		o.dim = 8
	}
	if o.key == "" {
		o.key = newKey()
	}
	if o.reg == nil {
		o.reg = registry.NewMemory()
	}
	h := &harness{t: t, reg: o.reg, key: o.key,
		llm: &countingLLM{}, emb: testutil.NewHashEmbedder(o.dim), vs: testutil.NewMemVectorStore(o.dim),
		store: persistence.NewMemoryStore(), queue: &testutil.MemQueue{}}
	if o.store != nil {
		h.store = o.store
	}
	pol, err := policy.NewManager("lru")
	if err != nil {
		t.Fatal(err)
	}
	h.slots = manager.NewSlots(h.llm, h.emb, h.vs, h.store, h.queue, pol)
	box, err := secrets.NewBox(o.key)
	if err != nil {
		t.Fatal(err)
	}
	mc := manager.Config{Enabled: true, AllowInsecureEndpoints: true, HealthTimeout: 10 * time.Second,
		RollbackWindow: time.Minute, QueueDrainWindow: 3 * time.Second, RestoreWait: 100 * time.Millisecond}
	if o.cfg != nil {
		o.cfg(&mc)
	}
	h.m = manager.New(mc, h.reg, box, h.slots, o.ctl)
	if err := h.m.Restore(context.Background()); err != nil {
		t.Fatalf("restore: %v", err)
	}
	cfg := config.Config{TopK: 1, Threshold: 0.25, Capacity: 100, Policy: "lru", LLMMode: "test"}
	h.svc, err = orchestrator.NewWithDependencies(cfg, orchestrator.Dependencies{
		Embedder: h.slots.Embedder, VectorStore: h.slots.VectorStore, LLM: h.slots.LLM,
		Store: h.slots.Store, Queue: h.slots.Queue, Policy: h.slots.Policy,
	})
	if err != nil {
		t.Fatal(err)
	}
	if th := h.m.RestoredThreshold(); th != nil {
		h.svc.SetThreshold(*th)
	}
	h.m.SetHost(h.svc)
	h.srv = httptest.NewServer(h.svc.Routes())
	t.Cleanup(h.srv.Close)
	t.Cleanup(h.m.Close)
	return h
}

type queryResp struct {
	Reply    string  `json:"reply"`
	CacheHit bool    `json:"cache_hit"`
	Source   string  `json:"source"`
	Distance float64 `json:"distance"`
}

func (h *harness) query(prompt string) queryResp {
	h.t.Helper()
	body, _ := json.Marshal(map[string]string{"prompt": prompt})
	resp, err := http.Post(h.srv.URL+"/query", "application/json", bytes.NewReader(body))
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out queryResp
	if resp.StatusCode != 200 {
		h.t.Fatalf("query %q: HTTP %d", prompt, resp.StatusCode)
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return out
}

// seed asks n distinct prompts and waits for them to be cached.
func (h *harness) seed(prompts ...string) {
	h.t.Helper()
	for _, p := range prompts {
		h.query(p)
	}
	h.waitCacheSize(len(prompts))
}

func (h *harness) waitCacheSize(n int) {
	h.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if got, _ := h.svc.CacheSize(); got == n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	got, _ := h.svc.CacheSize()
	h.t.Fatalf("cache size = %d, want %d", got, n)
}

func (h *harness) waitState(id string, want ...plugins.State) *registry.Record {
	h.t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		rec, err := h.m.Get(id)
		if err != nil {
			h.t.Fatal(err)
		}
		for _, w := range want {
			if rec.State == w {
				return rec
			}
		}
		if rec.State == plugins.StateFailed {
			h.t.Fatalf("plugin failed: %s\nlog: %v", rec.StateDetail, h.logs(id))
		}
		time.Sleep(20 * time.Millisecond)
	}
	rec, _ := h.m.Get(id)
	h.t.Fatalf("plugin stuck in %s (%s), wanted %v", rec.State, rec.StateDetail, want)
	return nil
}

func (h *harness) logs(id string) []string {
	l, _ := h.reg.Logs(id, 100)
	return l
}

// install installs an endpoint plugin and waits for it to be verified.
func (h *harness) installEndpoint(typ plugins.Type, name, url string, extra func(*manager.InstallRequest)) string {
	h.t.Helper()
	req := manager.InstallRequest{Type: string(typ), Mode: "endpoint", Name: name, Version: "1.0.0", Endpoint: url}
	if extra != nil {
		extra(&req)
	}
	rec, err := h.m.Install(context.Background(), req)
	if err != nil {
		h.t.Fatalf("install %s: %v", name, err)
	}
	h.waitState(rec.ID, plugins.StateVerified)
	return rec.ID
}

func (h *harness) activate(id string, conf registry.Confirmations) (*manager.Result, error) {
	return h.m.Activate(context.Background(), id, conf)
}

func (h *harness) mustActivate(id string, conf registry.Confirmations) *manager.Result {
	h.t.Helper()
	res, err := h.activate(id, conf)
	if err != nil {
		h.t.Fatalf("activate: %v\nlog: %v", err, h.logs(id))
	}
	return res
}
