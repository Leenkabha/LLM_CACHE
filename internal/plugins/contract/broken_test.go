package contract_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/leenkabha/llm_cache/internal/plugins"
	"github.com/leenkabha/llm_cache/internal/plugins/contract"
	"github.com/leenkabha/llm_cache/internal/plugins/testutil"
)

// fault sits in front of a healthy example and corrupts one behaviour. A suite
// that still passes against it is not testing that behaviour.
type fault struct {
	// req may rewrite the request body or answer directly (return handled=true).
	req func(method, path string, body []byte, w http.ResponseWriter) (newBody []byte, handled bool)
	// resp may rewrite the upstream response.
	resp func(method, path string, status int, body []byte) (int, []byte)
}

func faulty(t *testing.T, upstream string, f fault) string {
	t.Helper()
	hc := &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if f.req != nil {
			nb, handled := f.req(r.Method, r.URL.Path, body, w)
			if handled {
				return
			}
			body = nb
		}
		up, _ := http.NewRequest(r.Method, upstream+r.URL.RequestURI(), bytes.NewReader(body))
		up.Header = r.Header.Clone()
		resp, err := hc.Do(up)
		if err != nil {
			http.Error(w, "upstream", http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		rb, _ := io.ReadAll(resp.Body)
		status := resp.StatusCode
		if f.resp != nil {
			status, rb = f.resp(r.Method, r.URL.Path, status, rb)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(rb)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func mutateJSON(body []byte, fn func(map[string]any)) []byte {
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		return body
	}
	fn(m)
	out, _ := json.Marshal(m)
	return out
}

func TestContractSuitesRejectBrokenPlugins(t *testing.T) {
	type tc struct {
		name    string
		example string
		typ     plugins.Type
		fault   fault
		want    string // a check that must fail
		target  func(*contract.Target)
	}
	only := func(path string, fn func(status int, body []byte) (int, []byte)) fault {
		return fault{resp: func(_, p string, s int, b []byte) (int, []byte) {
			if p == path {
				return fn(s, b)
			}
			return s, b
		}}
	}
	cases := []tc{
		{"llm server error", "llm", plugins.TypeLLM, only("/v1/complete", func(int, []byte) (int, []byte) { return 500, []byte(`{"error":"boom"}`) }), "reply", nil},
		{"llm empty reply", "llm", plugins.TypeLLM, only("/v1/complete", func(s int, _ []byte) (int, []byte) { return s, []byte(`{"reply":"   "}`) }), "reply", nil},
		{"llm malformed json", "llm", plugins.TypeLLM, only("/v1/complete", func(s int, _ []byte) (int, []byte) { return s, []byte(`{"reply":`) }), "reply", nil},
		{"llm oversized reply", "llm", plugins.TypeLLM, only("/v1/complete", func(s int, _ []byte) (int, []byte) {
			return s, []byte(`{"reply":"` + strings.Repeat("x", 2<<20) + `"}`)
		}), "reply", nil},
		{"llm unhealthy", "llm", plugins.TypeLLM, only("/health", func(int, []byte) (int, []byte) { return 503, []byte(`{}`) }), "health", nil},
		{"llm accepts blank prompt", "llm", plugins.TypeLLM, fault{req: func(_, p string, b []byte, w http.ResponseWriter) ([]byte, bool) {
			if p == "/v1/complete" && bytes.Contains(b, []byte(`"prompt":""`)) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"reply":"ok"}`))
				return nil, true
			}
			return b, false
		}}, "malformed_request_rejected", nil},
		{"embedder dim mismatch", "embedder", plugins.TypeEmbedder, only("/v1/embed", func(s int, b []byte) (int, []byte) {
			return s, mutateJSON(b, func(m map[string]any) { m["dim"] = 99 })
		}), "vector_shape_and_finite", nil},
		{"embedder not normalized", "embedder", plugins.TypeEmbedder, only("/v1/embed", func(s int, b []byte) (int, []byte) {
			return s, mutateJSON(b, func(m map[string]any) {
				v := m["vector"].([]any)
				for i := range v {
					v[i] = v[i].(float64) * 3
				}
			})
		}), "normalized_unit_length", nil},
		{"embedder nondeterministic", "embedder", plugins.TypeEmbedder, only("/v1/embed", func(s int, b []byte) (int, []byte) {
			return s, mutateJSON(b, func(m map[string]any) {
				v := m["vector"].([]any)
				for i := range v {
					v[i] = v[i].(float64) + rand.Float64()*0.3
				}
			})
		}), "deterministic_for_same_text", func(x *contract.Target) { x.AllowUnnormalized = true }},
		{"vector store ignores threshold", "vector-store", plugins.TypeVectorStore, fault{req: func(_, p string, b []byte, _ http.ResponseWriter) ([]byte, bool) {
			if p == "/v1/search" {
				return mutateJSON(b, func(m map[string]any) { m["threshold"] = 1e9 }), false
			}
			return b, false
		}}, "threshold_is_inclusive", nil},
		{"vector store wrong order", "vector-store", plugins.TypeVectorStore, only("/v1/search", func(s int, b []byte) (int, []byte) {
			return s, mutateJSON(b, func(m map[string]any) {
				ms, _ := m["matches"].([]any)
				for i, j := 0, len(ms)-1; i < j; i, j = i+1, j-1 {
					ms[i], ms[j] = ms[j], ms[i]
				}
			})
		}), "top_k_and_best_to_worst_order", nil},
		{"vector store rebuild is a no-op", "vector-store", plugins.TypeVectorStore, fault{req: func(_, p string, b []byte, w http.ResponseWriter) ([]byte, bool) {
			if p == "/v1/rebuild" {
				var in struct{ Entries []any }
				_ = json.Unmarshal(b, &in)
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]int{"restored": len(in.Entries)})
				return nil, true
			}
			return b, false
		}}, "rebuild_replaces_contents", nil},
		{"persistence loses a field", "persistence", plugins.TypePersistence, only("/v1/entries/", func(s int, b []byte) (int, []byte) { return s, b }), "", nil},
		{"persistence 500 for missing", "persistence", plugins.TypePersistence, fault{resp: func(m, p string, s int, b []byte) (int, []byte) {
			if m == http.MethodGet && strings.HasPrefix(p, "/v1/entries/") && s == 404 {
				return 500, []byte(`{"error":{"code":"backend"}}`)
			}
			return s, b
		}}, "not_found_is_distinct_from_failure", nil},
		{"persistence drops reply", "persistence", plugins.TypePersistence, fault{resp: func(m, p string, s int, b []byte) (int, []byte) {
			if m == http.MethodGet && strings.HasPrefix(p, "/v1/entries/") && s == 200 {
				return s, mutateJSON(b, func(x map[string]any) { x["reply"] = "" })
			}
			return s, b
		}}, "save_load_roundtrip_all_fields", nil},
		{"queue non-durable enqueue status", "queue", plugins.TypeQueue, only("/v1/jobs", func(s int, b []byte) (int, []byte) { return 200, b }), "enqueue_is_durable_and_lease_returns_payload", nil},
		{"queue accepts stale ack", "queue", plugins.TypeQueue, fault{resp: func(m, p string, s int, b []byte) (int, []byte) {
			if strings.HasSuffix(p, "/ack") && s == 409 {
				return 200, []byte(`{"status":"ok"}`)
			}
			return s, b
		}}, "visibility_timeout_redelivers_and_stale_ack_rejected", nil},
		{"policy flush ignored", "policy", plugins.TypePolicy, fault{req: func(_, p string, b []byte, w http.ResponseWriter) ([]byte, bool) {
			if p == "/v1/flush" {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{}`))
				return nil, true
			}
			return b, false
		}}, "flush_leaves_policy_reusable", nil},
	}
	for _, c := range cases {
		if c.want == "" {
			continue
		}
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			ex := testutil.StartExample(t, c.example, nil)
			url := faulty(t, ex.URL, c.fault)
			tg := contract.Target{Type: c.typ, Client: client(t, url, "")}
			if c.target != nil {
				c.target(&tg)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			rep := contract.Run(ctx, tg)
			for _, r := range rep.Results {
				if r.Name == c.want {
					if r.Status != "fail" {
						t.Fatalf("check %q was %q against a broken plugin; the suite would let this bug through", c.want, r.Status)
					}
					return
				}
			}
			t.Fatalf("check %q did not run; results: %+v", c.want, rep.Results)
		})
	}
}
