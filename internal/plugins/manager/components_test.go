package manager_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/leenkabha/llm_cache/internal/plugins"
	"github.com/leenkabha/llm_cache/internal/plugins/manager"
	"github.com/leenkabha/llm_cache/internal/plugins/registry"
	"github.com/leenkabha/llm_cache/internal/plugins/testutil"
)

// proxy forwards to upstream but lets a test intercept requests.
func proxy(t *testing.T, upstream string, intercept func(w http.ResponseWriter, r *http.Request, body []byte) (handled bool), tweak func(path string, body []byte) []byte) string {
	t.Helper()
	hc := &http.Client{Timeout: 20 * time.Second}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if intercept != nil && intercept(w, r, body) {
			return
		}
		up, _ := http.NewRequest(r.Method, upstream+r.URL.RequestURI(), bytes.NewReader(body))
		up.Header = r.Header.Clone()
		resp, err := hc.Do(up)
		if err != nil {
			http.Error(w, "upstream", 502)
			return
		}
		defer resp.Body.Close()
		rb, _ := io.ReadAll(resp.Body)
		if tweak != nil {
			rb = tweak(r.URL.Path, rb)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(rb)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func getJSON(t *testing.T, url string, out any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		t.Fatal(err)
	}
}

func pluginSize(t *testing.T, base string) int {
	var out struct {
		Size int `json:"size"`
	}
	getJSON(t, base+"/v1/size", &out)
	return out.Size
}

func requirement(t *testing.T, err error) *manager.RequirementError {
	t.Helper()
	var re *manager.RequirementError
	if !errors.As(err, &re) {
		t.Fatalf("error = %v, want a RequirementError", err)
	}
	return re
}

// ---- embedder ----------------------------------------------------------------

func TestEmbedderSameModelNeedsNoConfirmation(t *testing.T) {
	h := newHarness(t, opts{})
	h.seed("alpha bravo", "charlie delta", "echo foxtrot")
	ex := testutil.StartExample(t, "embedder", map[string]string{"CONFIG_DIM": "8"}) // same name + dim as the built-in
	id := h.installEndpoint(plugins.TypeEmbedder, "same-emb", ex.URL, nil)
	h.mustActivate(id, registry.Confirmations{})
	if r := h.query("alpha bravo"); !r.CacheHit {
		t.Fatalf("cache lost across an identical-model embedder swap: %+v", r)
	}
}

func TestEmbedderModelChangeRequiresExplicitChoice(t *testing.T) {
	h := newHarness(t, opts{})
	h.seed("alpha bravo", "charlie delta")
	ex := testutil.StartExample(t, "embedder", map[string]string{"CONFIG_DIM": "8", "CONFIG_MODEL_NAME": "other-model"})
	id := h.installEndpoint(plugins.TypeEmbedder, "other-emb", ex.URL, nil)

	_, err := h.activate(id, registry.Confirmations{})
	re := requirement(t, err)
	if re.Field != "embedder_compat" || len(re.Options) != 2 {
		t.Fatalf("requirement = %+v", re)
	}
	rec, _ := h.m.Get(id)
	if rec.State != plugins.StateVerified || rec.Active {
		t.Fatalf("asking for confirmation must not change state: %s", rec.State)
	}
	if n, _ := h.svc.CacheSize(); n != 2 {
		t.Fatalf("cache was touched without confirmation: %d entries", n)
	}
	if r := h.query("alpha bravo"); !r.CacheHit {
		t.Fatal("old embedder stopped serving")
	}
	if _, err := h.activate(id, registry.Confirmations{EmbedderCompat: "bogus"}); err == nil {
		t.Fatal("an unknown compat action was accepted")
	}
}

func TestEmbedderFlushConfirmation(t *testing.T) {
	h := newHarness(t, opts{})
	h.seed("alpha bravo", "charlie delta")
	ex := testutil.StartExample(t, "embedder", map[string]string{"CONFIG_DIM": "8", "CONFIG_MODEL_NAME": "other-model"})
	id := h.installEndpoint(plugins.TypeEmbedder, "flush-emb", ex.URL, nil)
	res := h.mustActivate(id, registry.Confirmations{EmbedderCompat: "flush"})
	if n, _ := h.svc.CacheSize(); n != 0 {
		t.Fatalf("cache size after flush = %d", n)
	}
	if size, _ := h.vs.Size(context.Background()); size != 0 {
		t.Fatalf("vector store not flushed: %d", size)
	}
	if len(res.Warnings) == 0 || !strings.Contains(strings.Join(res.Warnings, " "), "flushed") {
		t.Fatalf("warnings = %v", res.Warnings)
	}
	if r := h.query("alpha bravo"); r.CacheHit {
		t.Fatal("a flushed entry was served")
	}
}

// rotate shifts a vector by one position: still unit length and deterministic,
// but a different vector space from the built-in.
func rotateEmbeddings(path string, body []byte) []byte {
	if path != "/v1/embed" {
		return body
	}
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		return body
	}
	v, _ := m["vector"].([]any)
	if len(v) > 1 {
		v = append(v[1:], v[0])
		m["vector"] = v
	}
	out, _ := json.Marshal(m)
	return out
}

func TestEmbedderReembedKeepsCacheUsable(t *testing.T) {
	h := newHarness(t, opts{})
	prompts := []string{"alpha bravo", "charlie delta", "echo foxtrot golf"}
	h.seed(prompts...)
	before := map[string][]float64{}
	entries, _ := h.store.List()
	for _, e := range entries {
		before[e.ID] = e.Vector
	}

	ex := testutil.StartExample(t, "embedder", map[string]string{"CONFIG_DIM": "8", "CONFIG_MODEL_NAME": "rotated"})
	url := proxy(t, ex.URL, nil, rotateEmbeddings)
	id := h.installEndpoint(plugins.TypeEmbedder, "rot-emb", url, func(r *manager.InstallRequest) { r.Endpoint = url })
	h.mustActivate(id, registry.Confirmations{EmbedderCompat: "reembed"})

	entries, _ = h.store.List()
	if len(entries) != 3 {
		t.Fatalf("entries after reembed = %d", len(entries))
	}
	changed := 0
	for _, e := range entries {
		if strings.TrimSpace(e.Prompt) == "" || e.Reply == "" {
			t.Fatalf("entry lost its prompt or reply: %+v", e)
		}
		for i := range e.Vector {
			if e.Vector[i] != before[e.ID][i] {
				changed++
				break
			}
		}
	}
	if changed == 0 {
		t.Fatal("no stored vector changed, so nothing was re-embedded")
	}
	for _, p := range prompts {
		if r := h.query(p); !r.CacheHit {
			t.Fatalf("prompt %q lost its cache entry after reembed", p)
		}
	}
	if size, _ := h.vs.Size(context.Background()); size != 3 {
		t.Fatalf("vector store size = %d", size)
	}
	if h.svc.Threshold() != 0.25 {
		t.Fatalf("threshold left at %v after the forced-miss window", h.svc.Threshold())
	}
}

func TestEmbedderDimensionIncompatibleWithVectorStore(t *testing.T) {
	h := newHarness(t, opts{})
	ex := testutil.StartExample(t, "embedder", map[string]string{"CONFIG_DIM": "16"})
	id := h.installEndpoint(plugins.TypeEmbedder, "wide-emb", ex.URL, nil)
	_, err := h.activate(id, registry.Confirmations{EmbedderCompat: "flush"})
	var inc *manager.IncompatibleError
	if !errors.As(err, &inc) || !strings.Contains(inc.Reason, "dimension") && !strings.Contains(inc.Reason, "-dimensional") {
		t.Fatalf("err = %v", err)
	}
	if h.m.ActiveIDs()[plugins.TypeEmbedder] != "" {
		t.Fatal("an incompatible embedder became active")
	}
	if r := h.query("still works"); r.Source == "" {
		t.Fatal("stack broken by the refused activation")
	}
}

// ---- vector store ------------------------------------------------------------

func TestVectorStoreRebuildsFromPersistence(t *testing.T) {
	h := newHarness(t, opts{})
	prompts := []string{"alpha bravo", "charlie delta", "echo foxtrot golf"}
	h.seed(prompts...)
	ex := testutil.StartExample(t, "vector-store", map[string]string{"CONFIG_DIM": "8"})
	id := h.installEndpoint(plugins.TypeVectorStore, "sdk-vs", ex.URL, nil)
	h.mustActivate(id, registry.Confirmations{})

	if n := pluginSize(t, ex.URL); n != 3 {
		t.Fatalf("plugin holds %d vectors after rebuild, want 3", n)
	}
	for _, p := range prompts {
		if r := h.query(p); !r.CacheHit {
			t.Fatalf("no hit for %q through the new vector store", p)
		}
	}
	// New entries go to the plugin.
	h.query("brand new prompt words")
	h.waitCacheSize(4)
	if n := pluginSize(t, ex.URL); n != 4 {
		t.Fatalf("plugin size = %d after a new entry, want 4", n)
	}
	// Built-in store untouched for rollback.
	if n, _ := h.vs.Size(context.Background()); n != 3 {
		t.Fatalf("built-in vector store changed: %d", n)
	}
}

func TestVectorStoreRebuildFailureLeavesOldStoreActive(t *testing.T) {
	h := newHarness(t, opts{})
	h.seed("alpha bravo", "charlie delta")
	ex := testutil.StartExample(t, "vector-store", map[string]string{"CONFIG_DIM": "8"})
	var failRebuild atomic.Bool
	url := proxy(t, ex.URL, func(w http.ResponseWriter, r *http.Request, _ []byte) bool {
		if failRebuild.Load() && r.URL.Path == "/v1/rebuild" {
			http.Error(w, `{"error":"disk full"}`, 500)
			return true
		}
		return false
	}, nil)
	id := h.installEndpoint(plugins.TypeVectorStore, "flaky-vs", url, nil)
	failRebuild.Store(true) // verified fine; now rebuild breaks

	if _, err := h.activate(id, registry.Confirmations{}); err == nil {
		t.Fatal("activation succeeded although the rebuild failed")
	}
	if h.m.ActiveIDs()[plugins.TypeVectorStore] != "" {
		t.Fatal("the failed candidate became active")
	}
	if r := h.query("alpha bravo"); !r.CacheHit {
		t.Fatal("previous vector store stopped serving after a failed activation")
	}
	rec, _ := h.m.Get(id)
	if rec.State != plugins.StateFailed || !strings.Contains(rec.StateDetail, "rebuild") {
		t.Fatalf("record = %s %q", rec.State, rec.StateDetail)
	}
}

func TestMetricChangeRequiresThresholdConfirmationAndRollbackRestoresIt(t *testing.T) {
	h := newHarness(t, opts{})
	h.seed("alpha bravo")
	ex := testutil.StartExample(t, "vector-store", map[string]string{"CONFIG_DIM": "8", "CONFIG_METRIC": "euclidean"})
	id := h.installEndpoint(plugins.TypeVectorStore, "l2-vs", ex.URL, nil)

	_, err := h.activate(id, registry.Confirmations{})
	re := requirement(t, err)
	if re.Field != "threshold" || !strings.Contains(re.Warning, "scales differ") {
		t.Fatalf("requirement = %+v", re)
	}
	if h.svc.Threshold() != 0.25 {
		t.Fatal("threshold changed without confirmation")
	}
	bad := -1.0
	if _, err := h.activate(id, registry.Confirmations{Threshold: &bad}); err == nil {
		t.Fatal("a negative threshold was accepted")
	}
	th := 0.5
	res := h.mustActivate(id, registry.Confirmations{Threshold: &th})
	if h.svc.Threshold() != 0.5 {
		t.Fatalf("threshold = %v, want 0.5", h.svc.Threshold())
	}
	if !strings.Contains(strings.Join(res.Warnings, " "), "0.5") {
		t.Fatalf("warnings = %v", res.Warnings)
	}
	if r := h.query("alpha bravo"); !r.CacheHit {
		t.Fatalf("no hit under the new metric: %+v", r)
	}

	if _, err := h.m.Rollback(context.Background(), id, registry.Confirmations{}); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if h.svc.Threshold() != 0.25 {
		t.Fatalf("rollback left the threshold at %v", h.svc.Threshold())
	}
}

// ---- persistence -------------------------------------------------------------

func TestPersistenceNeedsExplicitChoiceThenMigrates(t *testing.T) {
	h := newHarness(t, opts{})
	prompts := []string{"alpha bravo", "charlie delta", "echo foxtrot golf"}
	h.seed(prompts...)
	ex := testutil.StartExample(t, "persistence", nil)
	id := h.installEndpoint(plugins.TypePersistence, "sdk-store", ex.URL, nil)

	_, err := h.activate(id, registry.Confirmations{})
	re := requirement(t, err)
	if re.Field != "persistence_compat" {
		t.Fatalf("requirement = %+v", re)
	}
	if n := pluginSize(t, ex.URL); n != 0 {
		t.Fatal("entries copied without confirmation")
	}

	h.mustActivate(id, registry.Confirmations{PersistenceCompat: "migrate"})
	if n := pluginSize(t, ex.URL); n != 3 {
		t.Fatalf("plugin holds %d entries after migration, want 3", n)
	}
	for _, p := range prompts {
		if r := h.query(p); !r.CacheHit {
			t.Fatalf("entry %q not served from the migrated store", p)
		}
	}
	h.query("a new prompt for the plugin")
	deadline := time.Now().Add(5 * time.Second)
	for pluginSize(t, ex.URL) != 4 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := pluginSize(t, ex.URL); n != 4 {
		t.Fatalf("new writes did not reach the plugin store: %d", n)
	}
	if n, _ := h.store.Size(); n != 3 {
		t.Fatalf("old store changed (%d entries); it must stay intact for rollback", n)
	}
}

func TestPersistenceMigrationFailureRollsBack(t *testing.T) {
	h := newHarness(t, opts{})
	h.seed("alpha bravo", "charlie delta", "echo foxtrot golf")
	ex := testutil.StartExample(t, "persistence", nil)
	var puts atomic.Int32
	var fail atomic.Bool
	url := proxy(t, ex.URL, func(w http.ResponseWriter, r *http.Request, _ []byte) bool {
		if fail.Load() && r.Method == http.MethodPut && puts.Add(1) == 2 {
			http.Error(w, `{"error":"quota"}`, 500)
			return true
		}
		return false
	}, nil)
	id := h.installEndpoint(plugins.TypePersistence, "flaky-store", url, nil)
	fail.Store(true)

	if _, err := h.activate(id, registry.Confirmations{PersistenceCompat: "migrate"}); err == nil {
		t.Fatal("activation succeeded although the migration failed")
	}
	if h.m.ActiveIDs()[plugins.TypePersistence] != "" {
		t.Fatal("candidate became active")
	}
	if n := pluginSize(t, ex.URL); n != 0 {
		t.Fatalf("partial migration left %d entries in the candidate", n)
	}
	if r := h.query("alpha bravo"); !r.CacheHit {
		t.Fatal("original store stopped serving")
	}
	if n, _ := h.store.Size(); n != 3 {
		t.Fatalf("original store changed: %d", n)
	}
}

func TestPersistenceRefusesNonEmptyMigrationTarget(t *testing.T) {
	h := newHarness(t, opts{})
	h.seed("alpha bravo")
	ex := testutil.StartExample(t, "persistence", nil)
	id := h.installEndpoint(plugins.TypePersistence, "dirty-store", ex.URL, nil)
	// Someone writes into the candidate after it was verified empty.
	body := `{"id":"x1","prompt":"p","reply":"r","vector":[1,0],"created_at":"2026-01-01T00:00:00Z"}`
	req, _ := http.NewRequest(http.MethodPut, ex.URL+"/v1/entries/x1", strings.NewReader(body))
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode > 299 {
		t.Fatalf("seeding the candidate: %v", err)
	}
	_, err := h.activate(id, registry.Confirmations{PersistenceCompat: "migrate"})
	var inc *manager.IncompatibleError
	if !errors.As(err, &inc) {
		t.Fatalf("err = %v, want IncompatibleError", err)
	}
	if n, _ := h.store.Size(); n != 1 {
		t.Fatal("old store changed")
	}
}

func TestPersistenceStartEmptyKeepsOldStoreData(t *testing.T) {
	h := newHarness(t, opts{})
	h.seed("alpha bravo", "charlie delta")
	ex := testutil.StartExample(t, "persistence", nil)
	id := h.installEndpoint(plugins.TypePersistence, "empty-store", ex.URL, nil)
	res := h.mustActivate(id, registry.Confirmations{PersistenceCompat: "empty"})
	if n, _ := h.svc.CacheSize(); n != 0 {
		t.Fatalf("cache not empty: %d", n)
	}
	if n, _ := h.store.Size(); n != 2 {
		t.Fatalf("the previous store must not be deleted, has %d", n)
	}
	if size, _ := h.vs.Size(context.Background()); size != 0 {
		t.Fatalf("vector store still holds %d orphaned vectors", size)
	}
	if !strings.Contains(strings.Join(res.Warnings, " "), "empty cache") {
		t.Fatalf("warnings = %v", res.Warnings)
	}
	// Rolling back to the built-in, which still holds its two entries, restores
	// them: the vector index and policy are rebuilt from it, nothing is lost.
	res2, err := h.m.Rollback(context.Background(), id, registry.Confirmations{})
	if err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if n, _ := h.svc.CacheSize(); n != 2 {
		t.Fatalf("cache size after rollback = %d, want the 2 original entries", n)
	}
	if r := h.query("alpha bravo"); !r.CacheHit {
		t.Fatalf("original entry not served after rollback: %+v", r)
	}
	if !strings.Contains(strings.Join(res2.Warnings, " "), "rebuilt") {
		t.Fatalf("warnings = %v", res2.Warnings)
	}
}

func TestPersistenceAdoptServesTheCandidatesOwnEntries(t *testing.T) {
	h := newHarness(t, opts{})
	h.seed("alpha bravo", "charlie delta")
	ex := testutil.StartExample(t, "persistence", nil)
	id := h.installEndpoint(plugins.TypePersistence, "adopt-store", ex.URL, nil)
	// The candidate store already has one entry (with a valid vector) after verification.
	vec, _ := h.emb.Embed(context.Background(), "unrelated words here")
	vj, _ := json.Marshal(vec)
	body := `{"id":"pre1","prompt":"unrelated words here","reply":"from candidate","vector":` + string(vj) + `,"created_at":"2026-01-01T00:00:00Z"}`
	req, _ := http.NewRequest(http.MethodPut, ex.URL+"/v1/entries/pre1", strings.NewReader(body))
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode > 299 {
		t.Fatalf("seed: %v", err)
	}
	_, err := h.activate(id, registry.Confirmations{})
	re := requirement(t, err)
	if len(re.Options) != 1 || re.Options[0] != "adopt" {
		t.Fatalf("options = %v", re.Options)
	}
	h.mustActivate(id, registry.Confirmations{PersistenceCompat: "adopt"})
	if r := h.query("unrelated words here"); !r.CacheHit || r.Reply != "from candidate" {
		t.Fatalf("candidate's own entry not served: %+v", r)
	}
	if r := h.query("alpha bravo"); r.CacheHit {
		t.Fatal("an entry of the previous store is still being served")
	}
	if n, _ := h.store.Size(); n != 2 {
		t.Fatalf("previous store must stay intact: %d", n)
	}
}

// ---- queue -------------------------------------------------------------------

func TestQueueSwitchLosesNoJobs(t *testing.T) {
	h := newHarness(t, opts{})
	ex := testutil.StartExample(t, "queue", nil)
	id := h.installEndpoint(plugins.TypeQueue, "sdk-queue", ex.URL, nil)

	// Hold cache writes so jobs pile up in the OLD queue.
	resume, err := h.svc.PauseWrites(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 6; i++ {
		h.query("old queue prompt number " + string(rune('a'+i)) + " unique")
	}
	h.mustActivate(id, registry.Confirmations{})
	for i := 0; i < 6; i++ {
		h.query("new queue prompt number " + string(rune('a'+i)) + " different")
	}
	resume()

	h.waitCacheSize(12) // 6 from the old queue (drained) + 6 through the plugin queue
	entries, _ := h.store.List()
	seen := map[string]int{}
	for _, e := range entries {
		seen[e.Prompt]++
	}
	if len(seen) != 12 {
		t.Fatalf("%d distinct prompts cached, want 12", len(seen))
	}
	for p, n := range seen {
		if n != 1 {
			t.Fatalf("prompt %q cached %d times", p, n)
		}
	}
	h.m.WaitIdle()
	if d, _ := h.queue.Depth(context.Background()); d != 0 {
		t.Fatalf("old queue still holds %d jobs", d)
	}
}

// ---- policy ------------------------------------------------------------------

func TestPolicyReplaysPersistedEntriesInCreationOrder(t *testing.T) {
	h := newHarness(t, opts{})
	h.seed("alpha bravo", "charlie delta", "echo foxtrot golf")
	entries, _ := h.store.List()
	oldest := entries[0]
	for _, e := range entries {
		if e.CreatedAt.Before(oldest.CreatedAt) {
			oldest = e
		}
	}
	ex := testutil.StartExample(t, "policy", nil)
	id := h.installEndpoint(plugins.TypePolicy, "sdk-lru", ex.URL, nil)
	res := h.mustActivate(id, registry.Confirmations{})
	if h.slots.Policy.Current() != "sdk-lru" {
		t.Fatalf("policy name = %q", h.slots.Policy.Current())
	}
	victim, ok := h.slots.Policy.Victim()
	if !ok || victim != oldest.ID {
		t.Fatalf("victim = %q, want the oldest persisted entry %q", victim, oldest.ID)
	}
	if !strings.Contains(strings.Join(res.Warnings, " "), "not persisted") {
		t.Fatalf("warnings must mention lost metadata: %v", res.Warnings)
	}
	if _, err := h.m.Deactivate(context.Background(), id, registry.Confirmations{}); err != nil {
		t.Fatal(err)
	}
	if h.slots.Policy.Current() != "lru" {
		t.Fatalf("policy after deactivation = %q", h.slots.Policy.Current())
	}
}

func TestPolicyReplayFailureKeepsOldPolicy(t *testing.T) {
	h := newHarness(t, opts{})
	h.seed("alpha bravo", "charlie delta")
	ex := testutil.StartExample(t, "policy", nil)
	var fail atomic.Bool
	url := proxy(t, ex.URL, func(w http.ResponseWriter, r *http.Request, _ []byte) bool {
		if fail.Load() && r.URL.Path == "/v1/events" {
			http.Error(w, `{"error":"broken"}`, 500)
			return true
		}
		return false
	}, nil)
	id := h.installEndpoint(plugins.TypePolicy, "flaky-policy", url, nil)
	fail.Store(true)
	if _, err := h.activate(id, registry.Confirmations{}); err == nil {
		t.Fatal("activation succeeded although the replay failed")
	}
	if h.slots.Policy.Current() != "lru" {
		t.Fatalf("policy = %q after a failed replay", h.slots.Policy.Current())
	}
}

// ---- restart -----------------------------------------------------------------

func TestActiveSelectionsSurviveRestart(t *testing.T) {
	h1 := newHarness(t, opts{})
	h1.seed("alpha bravo", "charlie delta")
	llmEx := testutil.StartExample(t, "llm", nil)
	vsEx := testutil.StartExample(t, "vector-store", map[string]string{"CONFIG_DIM": "8"})
	polEx := testutil.StartExample(t, "policy", nil)
	idLLM := h1.installEndpoint(plugins.TypeLLM, "r-llm", llmEx.URL, nil)
	idVS := h1.installEndpoint(plugins.TypeVectorStore, "r-vs", vsEx.URL, nil)
	idPol := h1.installEndpoint(plugins.TypePolicy, "r-pol", polEx.URL, nil)
	for _, id := range []string{idLLM, idVS, idPol} {
		h1.mustActivate(id, registry.Confirmations{})
	}

	// "Restart": a new orchestrator and manager over the same registry, key and
	// persisted cache entries, with fresh built-in components.
	h2 := newHarness(t, opts{reg: h1.reg, key: h1.key, store: h1.store})
	active := h2.m.ActiveIDs()
	if active[plugins.TypeLLM] != idLLM || active[plugins.TypeVectorStore] != idVS || active[plugins.TypePolicy] != idPol {
		t.Fatalf("selections after restart = %v", active)
	}
	if r := h2.query("alpha bravo"); !r.CacheHit {
		t.Fatalf("restored vector-store plugin was not rebuilt from persistence: %+v", r)
	}
	if r := h2.query("something never asked before"); !strings.Contains(r.Reply, "[sdk-llm") {
		t.Fatalf("restored LLM plugin not serving: %q", r.Reply)
	}
	if h2.slots.Policy.Current() != "sdk-lru" {
		t.Fatalf("policy after restart = %q", h2.slots.Policy.Current())
	}
	if n := pluginSize(t, vsEx.URL); n < 2 {
		t.Fatalf("plugin vector store not repopulated: %d", n)
	}
}

func TestRestoreWithWrongKeyOrDeadPlugin(t *testing.T) {
	h1 := newHarness(t, opts{})
	llmEx := testutil.StartExample(t, "llm", nil)
	rec, err := h1.m.Install(context.Background(), manager.InstallRequest{Type: "llm", Mode: "endpoint", Name: "k-llm", Endpoint: llmEx.URL,
		Secrets: map[string]string{manager.EndpointTokenSecret: "tok"}})
	if err != nil {
		t.Fatal(err)
	}
	// The example ignores the token unless PLUGIN_AUTH_TOKEN is set, so verification passes.
	h1.waitState(rec.ID, plugins.StateVerified)
	h1.mustActivate(rec.ID, registry.Confirmations{})

	// Wrong master key: a stateless slot falls back to the built-in and says why.
	h2 := newHarness(t, opts{reg: h1.reg, key: newKey(), store: h1.store})
	if h2.m.ActiveIDs()[plugins.TypeLLM] != "" {
		t.Fatal("plugin restored with the wrong key")
	}
	got, _ := h2.reg.Get(rec.ID)
	if got.State != plugins.StateFailed || !strings.Contains(got.StateDetail, "PLUGIN_SECRET_KEY") {
		t.Fatalf("record = %s %q; the error must name the master key", got.State, got.StateDetail)
	}
	if r := h2.query("works on the built-in"); !strings.HasPrefix(r.Reply, "builtin-reply") {
		t.Fatalf("fallback not serving: %q", r.Reply)
	}
}
