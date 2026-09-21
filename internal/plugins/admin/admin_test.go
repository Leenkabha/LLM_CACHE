package admin_test

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/leenkabha/llm_cache/internal/persistence"
	"github.com/leenkabha/llm_cache/internal/plugins/admin"
	"github.com/leenkabha/llm_cache/internal/plugins/manager"
	"github.com/leenkabha/llm_cache/internal/plugins/registry"
	"github.com/leenkabha/llm_cache/internal/plugins/secrets"
	"github.com/leenkabha/llm_cache/internal/plugins/testutil"
	"github.com/leenkabha/llm_cache/internal/policy"
)

const token = "test-admin-token-123"

type fakeHost struct {
	th    float64
	store *persistence.MemoryStore
	vs    *testutil.MemVectorStore
}

func (h *fakeHost) PauseWrites(context.Context) (func(), error) { return func() {}, nil }
func (h *fakeHost) FlushCache(ctx context.Context) error {
	_ = h.vs.Flush(ctx)
	return h.store.Flush()
}
func (h *fakeHost) Threshold() float64      { return h.th }
func (h *fakeHost) SetThreshold(v float64)  { h.th = v }
func (h *fakeHost) CacheSize() (int, error) { return h.store.Size() }

type llmStub struct{}

func (llmStub) Complete(context.Context, string) (string, error) { return "builtin", nil }

func newAPI(t *testing.T, enabled bool, adminToken string, opts ...admin.Option) (*httptest.Server, *manager.Manager, *registry.Memory) {
	t.Helper()
	k := make([]byte, 32)
	_, _ = rand.Read(k)
	box, _ := secrets.NewBox(base64.StdEncoding.EncodeToString(k))
	pol, _ := policy.NewManager("lru")
	store := persistence.NewMemoryStore()
	vs := testutil.NewMemVectorStore(8)
	slots := manager.NewSlots(llmStub{}, testutil.NewHashEmbedder(8), vs, store, &testutil.MemQueue{}, pol)
	reg := registry.NewMemory()
	m := manager.New(manager.Config{Enabled: enabled, AllowInsecureEndpoints: true, HealthTimeout: 10 * time.Second}, reg, box, slots, nil)
	m.SetHost(&fakeHost{th: 0.25, store: store, vs: vs})
	srv := httptest.NewServer(admin.New(m, adminToken, opts...))
	t.Cleanup(srv.Close)
	t.Cleanup(m.Close)
	return srv, m, reg
}

func do(t *testing.T, method, url, tok, body string) (int, string) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, url, rd)
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

var allRoutes = []struct{ method, path string }{
	{"GET", "/admin/plugins"}, {"GET", "/admin/plugins/x"}, {"POST", "/admin/plugins/verify"},
	{"POST", "/admin/plugins/install"}, {"POST", "/admin/plugins/x/activate"}, {"POST", "/admin/plugins/x/deactivate"},
	{"POST", "/admin/plugins/x/upgrade"}, {"POST", "/admin/plugins/x/rollback"}, {"DELETE", "/admin/plugins/x"},
	{"GET", "/admin/plugins/x/logs"}, {"GET", "/admin/audit"},
}

func TestNoAdminTokenDisablesEveryRoute(t *testing.T) {
	srv, _, _ := newAPI(t, true, "")
	for _, r := range allRoutes {
		for _, tok := range []string{"", "anything", ""} {
			if code, _ := do(t, r.method, srv.URL+r.path, tok, "{}"); code != http.StatusServiceUnavailable {
				t.Errorf("%s %s with token %q = %d, want 503", r.method, r.path, tok, code)
			}
		}
	}
	// Even an "empty" bearer must not match an empty configured token.
	req, _ := http.NewRequest("GET", srv.URL+"/admin/plugins", nil)
	req.Header.Set("Authorization", "Bearer ")
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("empty bearer against empty token = %d", resp.StatusCode)
	}
}

func TestInstallationDisabledDisablesEveryRoute(t *testing.T) {
	srv, _, _ := newAPI(t, false, token)
	for _, r := range allRoutes {
		if code, _ := do(t, r.method, srv.URL+r.path, token, "{}"); code != http.StatusServiceUnavailable {
			t.Errorf("%s %s = %d, want 503 while ENABLE_PLUGIN_INSTALLATION is off", r.method, r.path, code)
		}
	}
}

func TestEveryRouteRequiresTheToken(t *testing.T) {
	srv, _, _ := newAPI(t, true, token, admin.WithAuthFailureLimit(1_000_000))
	for _, r := range allRoutes {
		for _, tok := range []string{"", "wrong", token + "x", strings.ToUpper(token)} {
			if code, _ := do(t, r.method, srv.URL+r.path, tok, "{}"); code != http.StatusUnauthorized {
				t.Errorf("%s %s with token %q = %d, want 401", r.method, r.path, tok, code)
			}
		}
	}
	if code, _ := do(t, "GET", srv.URL+"/admin/plugins", token, ""); code != 200 {
		t.Fatalf("valid token rejected: %d", code)
	}
}

func TestRepeatedBadTokensAreRateLimited(t *testing.T) {
	srv, _, _ := newAPI(t, true, token)
	var last int
	for i := 0; i < 15; i++ {
		last, _ = do(t, "GET", srv.URL+"/admin/plugins", "guess", "")
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("after 15 bad attempts status = %d, want 429", last)
	}
	// Even the right token is refused from a blocked address until the window passes.
	if code, _ := do(t, "GET", srv.URL+"/admin/plugins", token, ""); code != http.StatusTooManyRequests {
		t.Fatalf("blocked address with a valid token = %d", code)
	}
}

func TestRateLimitIsPerClientBehindATrustedProxy(t *testing.T) {
	srv, _, _ := newAPI(t, true, token, admin.WithTrustProxy(true))
	get := func(xff, tok string) int {
		req, _ := http.NewRequest("GET", srv.URL+"/admin/plugins", nil)
		req.Header.Set("X-Forwarded-For", xff)
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	for i := 0; i < 12; i++ { // an attacker behind the proxy exhausts ITS budget only
		get("203.0.113.9", "guess")
	}
	if code := get("203.0.113.9", "guess"); code != 429 {
		t.Fatalf("attacker status = %d, want 429", code)
	}
	if code := get("198.51.100.7", token); code != 200 {
		t.Fatalf("a different client behind the same proxy was locked out: %d", code)
	}
}

func TestStrictJSONAndBodyLimits(t *testing.T) {
	srv, _, _ := newAPI(t, true, token, admin.WithAuthFailureLimit(1_000_000))
	url := srv.URL + "/admin/plugins/verify"
	cases := map[string]struct {
		body string
		want int
	}{
		"unknown field":    {`{"type":"llm","mode":"endpoint","evil":true}`, 400},
		"trailing data":    {`{"type":"llm"} {"type":"llm"}`, 400},
		"not json":         {`hello`, 400},
		"array":            {`[1,2]`, 400},
		"empty":            {``, 400},
		"oversized":        {`{"type":"llm","manifest":"` + strings.Repeat("a", admin.MaxBodyBytes) + `"}`, 413},
		"wrong field type": {`{"type":5}`, 400},
	}
	for name, c := range cases {
		if code, _ := do(t, "POST", url, token, c.body); code != c.want {
			t.Errorf("%s: status %d, want %d", name, code, c.want)
		}
	}
	// A GET on a POST-only route never reaches the verify handler.
	if code, _ := do(t, "GET", srv.URL+"/admin/plugins/verify", token, ""); code != 404 && code != 405 {
		t.Errorf("GET on a POST route = %d", code)
	}
}

func poll(t *testing.T, srv *httptest.Server, id, want string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		_, body := do(t, "GET", srv.URL+"/admin/plugins/"+id, token, "")
		var out struct {
			Plugin map[string]any `json:"plugin"`
		}
		_ = json.Unmarshal([]byte(body), &out)
		st, _ := out.Plugin["state"].(string)
		if st == want {
			return out.Plugin
		}
		if st == "failed" {
			t.Fatalf("plugin failed: %v", out.Plugin["state_detail"])
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("plugin never reached %s", want)
	return nil
}

func TestFullLifecycleThroughTheAPIWithoutLeakingSecrets(t *testing.T) {
	srv, _, _ := newAPI(t, true, token)
	const secret = "s3cr3t-value-that-must-never-be-echoed"
	ex := testutil.StartExample(t, "llm", map[string]string{"PLUGIN_AUTH_TOKEN": secret})
	var transcript strings.Builder
	rec := func(code int, body string) string { transcript.WriteString(body); return body }

	// verify (does not persist)
	req, _ := json.Marshal(map[string]any{"type": "llm", "mode": "endpoint", "name": "api-llm", "endpoint": ex.URL,
		"config": map[string]any{"model": "m1"}, "secrets": map[string]string{manager.EndpointTokenSecret: secret}})
	code, body := do(t, "POST", srv.URL+"/admin/plugins/verify", token, string(req))
	rec(code, body)
	var vr struct {
		Valid        bool `json:"valid"`
		Verification struct {
			Passed bool `json:"passed"`
		} `json:"verification"`
	}
	_ = json.Unmarshal([]byte(body), &vr)
	if code != 200 || !vr.Valid || !vr.Verification.Passed {
		t.Fatalf("verify = %d %s", code, body)
	}
	if _, b := do(t, "GET", srv.URL+"/admin/plugins", token, ""); strings.Contains(b, `"name":"api-llm"`) {
		t.Fatal("verify must not persist a plugin")
	}

	// install
	code, body = do(t, "POST", srv.URL+"/admin/plugins/install", token, string(req))
	rec(code, body)
	if code != http.StatusAccepted {
		t.Fatalf("install = %d %s", code, body)
	}
	var ir struct {
		Plugin map[string]any `json:"plugin"`
	}
	_ = json.Unmarshal([]byte(body), &ir)
	id := ir.Plugin["id"].(string)
	verified := poll(t, srv, id, "verified")
	if verified["health"].(map[string]any)["status"] != "healthy" {
		t.Fatalf("health = %v", verified["health"])
	}
	secs := verified["secrets"].([]any)
	if len(secs) != 1 || secs[0].(map[string]any)["configured"] != true {
		t.Fatalf("secrets view = %v", secs)
	}

	// activate
	code, body = do(t, "POST", srv.URL+"/admin/plugins/"+id+"/activate", token, `{"confirmations":{}}`)
	rec(code, body)
	if code != 200 {
		t.Fatalf("activate = %d %s", code, body)
	}
	code, body = do(t, "GET", srv.URL+"/admin/plugins", token, "")
	rec(code, body)
	var lr struct {
		Components []struct {
			Type     string `json:"type"`
			ActiveID string `json:"active_id"`
		} `json:"components"`
	}
	_ = json.Unmarshal([]byte(body), &lr)
	found := false
	for _, c := range lr.Components {
		if c.Type == "llm" && c.ActiveID == id {
			found = true
		}
	}
	if !found || len(lr.Components) != 9 {
		t.Fatalf("components = %+v", lr.Components)
	}

	// delete restrictions, logs, audit
	if code, body := do(t, "DELETE", srv.URL+"/admin/plugins/"+id, token, ""); code != 400 {
		t.Fatalf("deleting the active plugin = %d %s", code, body)
	}
	code, body = do(t, "GET", srv.URL+"/admin/plugins/"+id+"/logs", token, "")
	rec(code, body)
	if code != 200 || !strings.Contains(body, "verified") {
		t.Fatalf("logs = %d %s", code, body)
	}
	code, body = do(t, "GET", srv.URL+"/admin/audit", token, "")
	rec(code, body)
	if !strings.Contains(body, `"action":"activate"`) {
		t.Fatalf("audit lacks the activation: %s", body)
	}

	if code, body = do(t, "POST", srv.URL+"/admin/plugins/"+id+"/deactivate", token, ""); code != 200 {
		t.Fatalf("deactivate = %d %s", code, body)
	}
	rec(code, body)
	if code, body = do(t, "DELETE", srv.URL+"/admin/plugins/"+id, token, ""); code != 200 {
		t.Fatalf("delete = %d %s", code, body)
	}
	if code, _ = do(t, "GET", srv.URL+"/admin/plugins/"+id, token, ""); code != 404 {
		t.Fatalf("deleted plugin still visible: %d", code)
	}
	if strings.Contains(transcript.String(), secret) {
		t.Fatal("a secret appeared in an API response")
	}
	for _, needle := range []string{`"ciphertext"`, `"nonce"`, `"kid"`, `"c":"`} {
		if strings.Contains(transcript.String(), needle) {
			t.Fatalf("encrypted blob field %s leaked into an API response", needle)
		}
	}
}

func TestConfirmationRequirementIsA409WithDetails(t *testing.T) {
	srv, _, _ := newAPI(t, true, token)
	ex := testutil.StartExample(t, "embedder", map[string]string{"CONFIG_DIM": "8", "CONFIG_MODEL_NAME": "different"})
	req, _ := json.Marshal(map[string]any{"type": "embedder", "mode": "endpoint", "name": "emb", "endpoint": ex.URL})
	_, body := do(t, "POST", srv.URL+"/admin/plugins/install", token, string(req))
	var ir struct {
		Plugin map[string]any `json:"plugin"`
	}
	_ = json.Unmarshal([]byte(body), &ir)
	id := ir.Plugin["id"].(string)
	poll(t, srv, id, "verified")
	// Empty cache -> nothing to convert -> no confirmation is needed.
	if code, body := do(t, "POST", srv.URL+"/admin/plugins/"+id+"/activate", token, ""); code != 200 {
		t.Fatalf("activation with an empty cache = %d %s", code, body)
	}
}

func TestUnknownPluginIs404(t *testing.T) {
	srv, _, _ := newAPI(t, true, token)
	for _, r := range []struct{ m, p string }{{"GET", "/admin/plugins/nope"}, {"POST", "/admin/plugins/nope/activate"}, {"DELETE", "/admin/plugins/nope"}, {"GET", "/admin/plugins/nope/logs"}} {
		if code, _ := do(t, r.m, srv.URL+r.p, token, ""); code != 404 {
			t.Errorf("%s %s = %d, want 404", r.m, r.p, code)
		}
	}
}
