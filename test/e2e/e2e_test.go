// Package e2e holds the end-to-end acceptance test of the plugin platform. It runs
// against a real, already-running docker compose stack (see scripts/e2e_plugins.sh)
// and is skipped otherwise.
package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

var (
	stack = os.Getenv("E2E_STACK_URL")
	admin = os.Getenv("ADMIN_TOKEN")
)

type resp struct {
	Code int
	Body []byte
}

func (r resp) json(t *testing.T, v any) {
	t.Helper()
	if err := json.Unmarshal(r.Body, v); err != nil {
		t.Fatalf("HTTP %d: not JSON: %s", r.Code, r.Body)
	}
}

func do(t *testing.T, method, path string, body any) resp {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, stack+path, rd)
	if strings.HasPrefix(path, "/admin/") || path == "/flush" {
		req.Header.Set("Authorization", "Bearer "+admin)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	c := &http.Client{Timeout: 12 * time.Minute}
	r, err := c.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer r.Body.Close()
	b, _ := io.ReadAll(r.Body)
	return resp{r.StatusCode, b}
}

type plugin struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Version     string `json:"version"`
	Type        string `json:"type"`
	State       string `json:"state"`
	StateDetail string `json:"state_detail"`
	Active      bool   `json:"active"`
	ImageDigest string `json:"image_digest"`
	Commit      string `json:"commit"`
	Health      struct {
		Status string `json:"status"`
	} `json:"health"`
	Verification *struct {
		Passed  bool   `json:"passed"`
		Summary string `json:"summary"`
	} `json:"verification"`
}

func getPlugin(t *testing.T, id string) plugin {
	t.Helper()
	var out struct {
		Plugin plugin `json:"plugin"`
	}
	do(t, "GET", "/admin/plugins/"+id, nil).json(t, &out)
	return out.Plugin
}

func waitState(t *testing.T, id string, want string) plugin {
	t.Helper()
	deadline := time.Now().Add(12 * time.Minute)
	for time.Now().Before(deadline) {
		p := getPlugin(t, id)
		if p.State == want {
			return p
		}
		if p.State == "failed" {
			return p
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("plugin %s never reached %s", id, want)
	return plugin{}
}

func install(t *testing.T, req map[string]any) plugin {
	t.Helper()
	r := do(t, "POST", "/admin/plugins/install", req)
	if r.Code != http.StatusAccepted {
		t.Fatalf("install %v: HTTP %d %s", req["image"], r.Code, r.Body)
	}
	var out struct {
		Plugin plugin `json:"plugin"`
	}
	r.json(t, &out)
	return out.Plugin
}

// installVerified installs and waits until the candidate is verified.
func installVerified(t *testing.T, req map[string]any) plugin {
	t.Helper()
	p := install(t, req)
	p = waitState(t, p.ID, "verified")
	if p.State != "verified" {
		t.Fatalf("%s did not verify: %s", p.Name, p.StateDetail)
	}
	return p
}

type activation struct {
	Code     int
	Plugin   plugin
	Warnings []string
	Requires *struct {
		Field   string   `json:"field"`
		Options []string `json:"options"`
		Warning string   `json:"warning"`
	}
	Err string
}

func act(t *testing.T, id, op string, conf map[string]any) activation {
	t.Helper()
	body := map[string]any{}
	if conf != nil {
		body["confirmations"] = conf
	}
	r := do(t, "POST", "/admin/plugins/"+id+"/"+op, body)
	var out struct {
		Plugin   plugin   `json:"plugin"`
		Warnings []string `json:"warnings"`
		Error    string   `json:"error"`
		Requires *struct {
			Field   string   `json:"field"`
			Options []string `json:"options"`
			Warning string   `json:"warning"`
		} `json:"requires"`
	}
	r.json(t, &out)
	return activation{r.Code, out.Plugin, out.Warnings, out.Requires, out.Error}
}

func mustActivate(t *testing.T, id string, conf map[string]any) activation {
	t.Helper()
	a := act(t, id, "activate", conf)
	if a.Code != 200 {
		t.Fatalf("activate: HTTP %d %s (requires=%+v)", a.Code, a.Err, a.Requires)
	}
	return a
}

type queryOut struct {
	Reply    string `json:"reply"`
	CacheHit bool   `json:"cache_hit"`
	Source   string `json:"source"`
}

func query(t *testing.T, prompt string) queryOut {
	t.Helper()
	r := do(t, "POST", "/query", map[string]string{"prompt": prompt})
	if r.Code != 200 {
		t.Fatalf("query %q: HTTP %d %s", prompt, r.Code, r.Body)
	}
	var q queryOut
	r.json(t, &q)
	return q
}

func cacheSize(t *testing.T) int {
	var s struct {
		Size int `json:"size"`
	}
	do(t, "GET", "/stats", nil).json(t, &s)
	return s.Size
}

func waitSize(t *testing.T, want int) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if cacheSize(t) == want {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("cache size = %d, want %d", cacheSize(t), want)
}

func activeIDs(t *testing.T) map[string]string {
	var out struct {
		Active map[string]string `json:"active"`
	}
	do(t, "GET", "/admin/plugins", nil).json(t, &out)
	return out.Active
}

func compose(t *testing.T, args ...string) string {
	t.Helper()
	parts := strings.Fields(os.Getenv("E2E_COMPOSE"))
	if len(parts) == 0 {
		t.Skip("E2E_COMPOSE not set")
	}
	cmd := exec.Command(parts[0], append(parts[1:], args...)...)
	cmd.Dir = "../.." // the compose files are addressed relative to the repository root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %v\n%s", args, err, out)
	}
	return string(out)
}

// topics are prompts about unrelated subjects. They must be unrelated twice over: the
// stack starts with the real sentence-transformers model, which treats similar-looking
// strings as similar, and the test embedder is a bag of words, which treats prompts that
// share words as similar. Distinct topics with disjoint content words keep every prompt a
// separate cache entry under both, so cache-size assertions are exact.
var topics = []string{
	"How do volcanoes erupt", "Recipe for sourdough bread", "Best way to learn the violin", "Why is the ocean salty",
	"Explain quantum entanglement simply", "Tips for growing tomatoes indoors", "History of Roman aqueducts", "How does a refrigerator work",
	"Causes of the French Revolution", "Training plan for a marathon", "What is blockchain consensus", "Symptoms of vitamin deficiency",
	"Origins of jazz music", "How do vaccines train immunity", "Planning a budget for retirement", "Photosynthesis in desert plants",
	"Architecture of Gothic cathedrals", "Repairing a leaking faucet", "Migration patterns of monarch butterflies", "Chess opening strategies",
	"Meaning of Shakespeare sonnets", "Choosing a mountain bike", "How airplanes stay aloft", "Basics of impressionist painting",
	"Brewing craft beer at home", "Discovery of penicillin", "Solar panel installation costs", "Building Egyptian pyramids",
	"Preventing cyber phishing attacks", "Yoga poses for back pain", "Deep sea hydrothermal vents", "Knitting a wool sweater",
	"Rules of cricket", "Tokyo street food guide", "Black holes and gravity waves", "Composting kitchen scraps",
	"Origami crane instructions", "Dinosaur extinction theories", "Negotiating a salary raise", "Telescope buying advice",
}

var promptCounter int

// fresh returns the next unused, unrelated prompt.
func fresh() string {
	if promptCounter >= len(topics) {
		panic("the acceptance test ran out of unrelated prompts")
	}
	promptCounter++
	return topics[promptCounter-1]
}

const apiSecret = "e2e-secret-value-9f3a1c7d5b2e"

func TestPluginPlatformAcceptance(t *testing.T) {
	if stack == "" || admin == "" {
		t.Skip("set E2E_STACK_URL and ADMIN_TOKEN (scripts/e2e_plugins.sh does)")
	}
	warmPrompt, llmPrompt := fresh(), fresh()
	var transcript strings.Builder // every admin API body, scanned for secrets at the end
	record := func(b []byte) { transcript.Write(b) }

	// ---- 1. the normal stack is up; plugin management is authenticated -------------------
	t.Run("stack_is_up_and_management_requires_the_token", func(t *testing.T) {
		if r := do(t, "GET", "/health", nil); r.Code != 200 {
			t.Fatalf("health: %d %s", r.Code, r.Body)
		}
		req, _ := http.NewRequest("GET", stack+"/admin/plugins", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil || resp.StatusCode != 401 {
			t.Fatalf("unauthenticated /admin/plugins = %v %v", resp, err)
		}
		var list struct {
			Components []struct{ Type string } `json:"components"`
		}
		r := do(t, "GET", "/admin/plugins", nil)
		record(r.Body)
		r.json(t, &list)
		if len(list.Components) != 9 {
			t.Fatalf("components = %d, want all 9 plugin types", len(list.Components))
		}
		if q := query(t, warmPrompt); q.Source != "llm" {
			t.Fatalf("first query = %+v", q)
		}
		waitSize(t, 1)
	})

	// ---- 2. custom LLM (prebuilt image, with a runtime secret) ---------------------------------
	var llmV1 plugin
	t.Run("custom_llm_is_used_on_a_miss_and_repeat_is_cached", func(t *testing.T) {
		llmV1 = installVerified(t, map[string]any{
			"type": "llm", "mode": "image", "image": "llmcache-e2e/llm:1",
			"config": map[string]any{"model": "demo"}, "secrets": map[string]string{"API_TOKEN": apiSecret},
			"manifest": manifestFor("llm"),
		})
		if !strings.HasPrefix(llmV1.ImageDigest, "sha256:") {
			t.Fatalf("image not pinned: %+v", llmV1)
		}
		mustActivate(t, llmV1.ID, nil)
		q := query(t, llmPrompt)
		if q.CacheHit || !strings.Contains(q.Reply, "[sdk-llm demo secret-configured]") {
			t.Fatalf("an uncached request did not reach the plugin: %+v", q)
		}
		waitSize(t, 2)
		if q2 := query(t, llmPrompt); !q2.CacheHit || q2.Reply != q.Reply {
			t.Fatalf("repeat was not cached: %+v", q2)
		}
	})

	// A pool of prompts cached so far, used to prove state survives each migration.
	prompts := []string{warmPrompt, llmPrompt}
	assertCached := func(t *testing.T, why string) {
		t.Helper()
		for _, p := range prompts {
			if q := query(t, p); !q.CacheHit {
				t.Fatalf("%s: %q is no longer cached (%+v)", why, p, q)
			}
		}
	}
	addPrompts := func(t *testing.T, ps ...string) {
		t.Helper()
		for _, p := range ps {
			query(t, p)
			prompts = append(prompts, p)
		}
		waitSize(t, len(prompts))
	}

	// ---- 3. custom embedder with an explicit compatibility action --------------------------------
	t.Run("custom_embedder_needs_and_uses_an_explicit_compatibility_action", func(t *testing.T) {
		p := installVerified(t, map[string]any{"type": "embedder", "mode": "image", "image": "llmcache-e2e/embedder:1",
			"manifest": manifestFor("embedder"), "config": map[string]any{"dim": 384}})
		a := act(t, p.ID, "activate", nil)
		if a.Code != 409 || a.Requires == nil || a.Requires.Field != "embedder_compat" {
			t.Fatalf("model change was not blocked for confirmation: %d %+v %s", a.Code, a.Requires, a.Err)
		}
		if n := cacheSize(t); n != len(prompts) {
			t.Fatalf("the cache changed without confirmation: %d", n)
		}
		mustActivate(t, p.ID, map[string]any{"embedder_compat": "reembed"})
		assertCached(t, "after re-embedding with the custom embedder")
	})

	// ---- 4. rebuild into a custom vector store ------------------------------------------------------
	t.Run("rebuild_into_a_custom_vector_store", func(t *testing.T) {
		p := installVerified(t, map[string]any{"type": "vector-store", "mode": "image", "image": "llmcache-e2e/vector-store:1",
			"manifest": manifestFor("vector-store"), "config": map[string]any{"dim": 384, "metric": "cosine"}})
		mustActivate(t, p.ID, nil)
		assertCached(t, "after rebuilding into the custom vector store")
		addPrompts(t, fresh())
	})

	// ---- 5. migrate to a custom persistence plugin -------------------------------------------------------
	t.Run("migrate_to_a_custom_persistence_plugin", func(t *testing.T) {
		p := installVerified(t, map[string]any{"type": "persistence", "mode": "image", "image": "llmcache-e2e/persistence:1", "manifest": manifestFor("persistence")})
		a := act(t, p.ID, "activate", nil)
		if a.Code != 409 || a.Requires == nil || a.Requires.Field != "persistence_compat" {
			t.Fatalf("existing entries were not protected: %d %+v", a.Code, a.Requires)
		}
		before := cacheSize(t)
		mustActivate(t, p.ID, map[string]any{"persistence_compat": "migrate"})
		if after := cacheSize(t); after != before {
			t.Fatalf("entry count changed across the migration: %d -> %d", before, after)
		}
		assertCached(t, "after migrating persistence")
		addPrompts(t, fresh())
	})

	// ---- 6. switch to a custom queue without losing a job -------------------------------------------------
	t.Run("switch_to_a_custom_queue_without_losing_a_job", func(t *testing.T) {
		p := installVerified(t, map[string]any{"type": "queue", "mode": "image", "image": "llmcache-e2e/queue:1", "manifest": manifestFor("queue")})
		for i := 0; i < 8; i++ { // jobs in flight in the built-in Redis queue
			pr := fresh()
			query(t, pr)
			prompts = append(prompts, pr)
		}
		mustActivate(t, p.ID, nil)
		for i := 0; i < 8; i++ { // jobs through the plugin queue
			pr := fresh()
			query(t, pr)
			prompts = append(prompts, pr)
		}
		waitSize(t, len(prompts)) // nothing lost, nothing duplicated
	})

	// ---- 7. custom policy after replaying persisted entries ---------------------------------------------------
	t.Run("switch_to_a_custom_policy_after_replay", func(t *testing.T) {
		p := installVerified(t, map[string]any{"type": "policy", "mode": "image", "image": "llmcache-e2e/policy:1", "manifest": manifestFor("policy")})
		a := mustActivate(t, p.ID, nil)
		if !strings.Contains(strings.Join(a.Warnings, " "), "not persisted") {
			t.Fatalf("the lost recency metadata was not reported: %v", a.Warnings)
		}
		var st struct {
			Policy string `json:"policy"`
		}
		do(t, "GET", "/stats", nil).json(t, &st)
		if st.Policy != "sdk-lru" {
			t.Fatalf("policy = %q", st.Policy)
		}
	})

	// ---- 8-10. Python plugins built into isolated runner images --------------------------------------------------
	var embModel, vecIndex, metric plugin
	t.Run("python_embedding_model_plugin", func(t *testing.T) {
		embModel = installVerified(t, map[string]any{"type": "embedding-model", "mode": "repository", "repo": "/e2e-repos/embedding-model",
			"revision": "main", "config": map[string]any{"dim": 384}})
		if len(embModel.Commit) != 40 {
			t.Fatalf("commit not recorded: %+v", embModel)
		}
		mustActivate(t, embModel.ID, map[string]any{"embedder_compat": "reembed"})
		assertCached(t, "after activating the Python embedding model")
	})
	t.Run("python_vector_index_plugin", func(t *testing.T) {
		vecIndex = installVerified(t, map[string]any{"type": "vector-index", "mode": "repository", "repo": "/e2e-repos/vector-index", "revision": "main"})
		mustActivate(t, vecIndex.ID, nil)
		assertCached(t, "after activating the Python vector index")
	})
	t.Run("python_similarity_metric_plugin", func(t *testing.T) {
		metric = installVerified(t, map[string]any{"type": "similarity-metric", "mode": "repository", "repo": "/e2e-repos/similarity-metric", "revision": "main"})
		a := act(t, metric.ID, "activate", nil)
		if a.Code != 409 || a.Requires == nil || a.Requires.Field != "threshold" || !strings.Contains(a.Requires.Warning, "scales differ") {
			t.Fatalf("metric change did not require a threshold confirmation: %d %+v", a.Code, a.Requires)
		}
		mustActivate(t, metric.ID, map[string]any{"threshold": 0.1})
		assertCached(t, "after activating the Python similarity metric")
	})

	// ---- 11. a broken plugin never becomes active and the working one stays ----------------------------------------------
	t.Run("broken_plugins_leave_the_working_plugin_active", func(t *testing.T) {
		before := activeIDs(t)
		// (a) fails contract verification: a queue image installed as an LLM.
		bad := install(t, map[string]any{"type": "llm", "mode": "image", "image": "llmcache-e2e/queue:1", "manifest": manifestFor("llm")})
		bad = waitState(t, bad.ID, "verified")
		if bad.State != "failed" || bad.Verification == nil || bad.Verification.Passed {
			t.Fatalf("a plugin that violates the contract was not rejected: %+v", bad)
		}
		if r := act(t, bad.ID, "activate", nil); r.Code == 200 {
			t.Fatal("a failed plugin was activated")
		}
		// (b) verifies, but cannot be activated: its vectors do not fit the active embedder.
		narrow := installVerified(t, map[string]any{"type": "vector-store", "mode": "image", "image": "llmcache-e2e/vector-store:1",
			"manifest": manifestFor("vector-store"), "config": map[string]any{"dim": 16}})
		r := act(t, narrow.ID, "activate", nil)
		if r.Code != 422 || !strings.Contains(r.Err, "dimensional") {
			t.Fatalf("activation of an incompatible plugin = %d %q", r.Code, r.Err)
		}
		if after := activeIDs(t); fmt.Sprint(after) != fmt.Sprint(before) {
			t.Fatalf("active selection changed: %v -> %v", before, after)
		}
		assertCached(t, "after refused activations")
		pr := fresh()
		if q := query(t, pr); q.Reply == "" {
			t.Fatal("stack stopped answering")
		}
		prompts = append(prompts, pr)
		waitSize(t, len(prompts))
	})

	// ---- 12. upgrade, then roll back ---------------------------------------------------------------------------------------
	t.Run("roll_back_an_upgraded_plugin", func(t *testing.T) {
		v2m := strings.Replace(manifestFor("llm"), "version: 1.0.0", "version: 1.1.0", 1)
		r := do(t, "POST", "/admin/plugins/"+llmV1.ID+"/upgrade", map[string]any{
			"mode": "image", "image": "llmcache-e2e/llm:1", "manifest": v2m, "version": "1.1.0",
			"config": map[string]any{"model": "demo-v2"}})
		if r.Code != http.StatusAccepted {
			t.Fatalf("upgrade: %d %s", r.Code, r.Body)
		}
		var up struct {
			Plugin plugin `json:"plugin"`
		}
		r.json(t, &up)
		p := waitState(t, up.Plugin.ID, "verified")
		if p.State != "verified" {
			t.Fatalf("upgrade did not verify: %s", p.StateDetail)
		}
		mustActivate(t, p.ID, nil)
		pr := fresh()
		q := query(t, pr)
		if !strings.Contains(q.Reply, "[sdk-llm demo-v2 secret-configured]") {
			t.Fatalf("upgraded plugin not serving (its secret must be carried over): %q", q.Reply)
		}
		prompts = append(prompts, pr)
		waitSize(t, len(prompts))
		res := act(t, p.ID, "rollback", nil)
		if res.Code != 200 || res.Plugin.ID != llmV1.ID {
			t.Fatalf("rollback: %d %+v %s", res.Code, res.Plugin, res.Err)
		}
		pr = fresh()
		q = query(t, pr)
		if !strings.Contains(q.Reply, "[sdk-llm demo secret-configured]") {
			t.Fatalf("rollback did not restore v1: %q", q.Reply)
		}
		prompts = append(prompts, pr)
		waitSize(t, len(prompts))
	})

	// ---- 13. restart the whole stack; selections are restored ------------------------------------------------------------------------
	t.Run("restart_the_stack_and_restore_active_selections", func(t *testing.T) {
		before := activeIDs(t)
		size := cacheSize(t)
		compose(t, "restart")
		deadline := time.Now().Add(6 * time.Minute)
		for {
			r, err := http.Get(stack + "/health")
			if err == nil {
				b, _ := io.ReadAll(r.Body)
				r.Body.Close()
				if r.StatusCode == 200 && strings.Contains(string(b), `"ok"`) {
					break
				}
			}
			if time.Now().After(deadline) {
				logs := compose(t, "logs", "--tail=60", "orchestrator")
				t.Fatalf("stack did not come back healthy:\n%s", logs)
			}
			time.Sleep(3 * time.Second)
		}
		after := activeIDs(t)
		if fmt.Sprint(before) != fmt.Sprint(after) {
			t.Fatalf("active selections were not restored:\n before %v\n after  %v", before, after)
		}
		if n := cacheSize(t); n != size {
			t.Fatalf("cache size %d -> %d across the restart", size, n)
		}
		assertCached(t, "after the restart")
		if q := query(t, fresh()); !strings.Contains(q.Reply, "[sdk-llm demo") {
			t.Fatalf("restored LLM plugin not serving: %q", q.Reply)
		}
	})

	// ---- 14. no API response or log exposes a secret -------------------------------------------------------------------------------------
	t.Run("no_api_or_log_exposes_secrets", func(t *testing.T) {
		var list struct {
			Plugins []plugin `json:"plugins"`
		}
		r := do(t, "GET", "/admin/plugins", nil)
		record(r.Body)
		r.json(t, &list)
		for _, p := range list.Plugins {
			b := do(t, "GET", "/admin/plugins/"+p.ID, nil)
			record(b.Body)
			l := do(t, "GET", "/admin/plugins/"+p.ID+"/logs?limit=500", nil)
			record(l.Body)
		}
		record(do(t, "GET", "/admin/audit?limit=500", nil).Body)
		for _, secret := range []string{apiSecret, admin, os.Getenv("PLUGIN_SECRET_KEY"), os.Getenv("PLUGIN_CONTROLLER_TOKEN")} {
			if secret != "" && strings.Contains(transcript.String(), secret) {
				t.Fatalf("a secret appeared in an admin API response")
			}
		}
		for _, needle := range []string{`"ciphertext"`, `"nonce"`, `"kid"`} {
			if strings.Contains(transcript.String(), needle) {
				t.Fatalf("encrypted blob field %s appeared in an API response", needle)
			}
		}
		logs := compose(t, "logs", "--no-color", "orchestrator", "plugin-controller")
		for _, secret := range []string{apiSecret, os.Getenv("PLUGIN_SECRET_KEY"), os.Getenv("PLUGIN_CONTROLLER_TOKEN")} {
			if secret != "" && strings.Contains(logs, secret) {
				t.Fatalf("a secret appeared in the service logs")
			}
		}
		// The registry in Redis holds only ciphertext.
		keys := compose(t, "exec", "-T", "redis", "redis-cli", "--scan", "--pattern", "llm_cache_plugins:*")
		if !strings.Contains(keys, "llm_cache_plugins:record:") {
			t.Fatalf("no plugin records found in Redis: %s", keys)
		}
		dump := ""
		for _, k := range strings.Fields(keys) {
			if strings.Contains(k, ":record:") {
				dump += compose(t, "exec", "-T", "redis", "redis-cli", "GET", k)
			}
		}
		if strings.Contains(dump, apiSecret) {
			t.Fatal("a plugin secret is stored in plaintext in Redis")
		}
		if !strings.Contains(dump, `"kid"`) {
			t.Fatal("expected encrypted secret blobs in the registry")
		}
		// The cache namespace is separate from the plugin registry namespace.
		if strings.Contains(compose(t, "exec", "-T", "redis", "redis-cli", "--scan", "--pattern", "llm_cache:*"), "llm_cache_plugins") {
			t.Fatal("plugin data leaked into the cache namespace")
		}
	})
}

// manifestFor returns the SDK example's plugin.yaml (used for prebuilt images).
func manifestFor(example string) string {
	b, err := os.ReadFile("../../sdk/examples/" + example + "/plugin.yaml")
	if err != nil {
		panic(err)
	}
	return string(b)
}
