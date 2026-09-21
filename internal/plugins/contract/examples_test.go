package contract_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/leenkabha/llm_cache/internal/plugins"
	"github.com/leenkabha/llm_cache/internal/plugins/contract"
	"github.com/leenkabha/llm_cache/internal/plugins/protocol"
	"github.com/leenkabha/llm_cache/internal/plugins/safehttp"
	"github.com/leenkabha/llm_cache/internal/plugins/testutil"
)

func client(t *testing.T, base, token string) *protocol.Client {
	t.Helper()
	c, err := protocol.NewClient(base, safehttp.NewClient(safehttp.Policy{AllowInsecure: true}, 20*time.Second), token)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func report(t *testing.T, rep contract.Report) {
	t.Helper()
	for _, r := range rep.Results {
		switch r.Status {
		case "fail":
			t.Errorf("FAIL %s: %s", r.Name, r.Detail)
		case "skip":
			t.Logf("skip %s: %s", r.Name, r.Detail)
		}
	}
	if !rep.Passed() {
		t.Fatalf("%s", rep.Summary())
	}
	t.Log(rep.Summary())
}

// Every SDK example must pass the contract suite for its type.
func TestSDKExamplesPassTheirContractSuites(t *testing.T) {
	cases := []struct {
		example string
		typ     plugins.Type
		env     map[string]string
		target  func(*contract.Target)
	}{
		{"llm", plugins.TypeLLM, map[string]string{"API_TOKEN": "s3cret-token-value"}, func(x *contract.Target) { x.Model = "demo" }},
		{"embedder", plugins.TypeEmbedder, nil, nil},
		{"vector-store", plugins.TypeVectorStore, nil, func(x *contract.Target) { x.RequireEmpty = true }},
		{"persistence", plugins.TypePersistence, nil, func(x *contract.Target) { x.RequireEmpty = true }},
		{"queue", plugins.TypeQueue, nil, nil},
		{"policy", plugins.TypePolicy, nil, nil},
	}
	for _, c := range cases {
		c := c
		t.Run(c.example, func(t *testing.T) {
			t.Parallel()
			ex := testutil.StartExample(t, c.example, c.env)
			tg := contract.Target{Type: c.typ, Client: client(t, ex.URL, "")}
			if c.target != nil {
				c.target(&tg)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			report(t, contract.Run(ctx, tg))
		})
	}
}

func TestSDKExamplesEnforcePluginAuthToken(t *testing.T) {
	ex := testutil.StartExample(t, "llm", map[string]string{"PLUGIN_AUTH_TOKEN": "tok-123"})
	ctx := context.Background()
	if err := client(t, ex.URL, "").Health(ctx); err != nil {
		t.Fatalf("health must stay open: %v", err)
	}
	if _, err := client(t, ex.URL, "").Do(ctx, http.MethodPost, "/v1/complete", map[string]string{"model": "m", "prompt": "hi"}, nil); err == nil {
		t.Fatal("unauthenticated request accepted")
	}
	tg := contract.Target{Type: plugins.TypeLLM, Client: client(t, ex.URL, "tok-123")}
	report(t, contract.Run(ctx, tg))
}
