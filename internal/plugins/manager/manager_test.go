package manager_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/leenkabha/llm_cache/internal/plugins"
	"github.com/leenkabha/llm_cache/internal/plugins/manager"
	"github.com/leenkabha/llm_cache/internal/plugins/registry"
	"github.com/leenkabha/llm_cache/internal/plugins/testutil"
)

func TestDisabledManagerRefusesEverything(t *testing.T) {
	h := newHarness(t, opts{cfg: func(c *manager.Config) { c.Enabled = false }})
	ctx := context.Background()
	if _, err := h.m.Install(ctx, manager.InstallRequest{Type: "llm", Mode: "endpoint", Endpoint: "http://127.0.0.1:1"}); !errors.Is(err, manager.ErrDisabled) {
		t.Fatalf("Install: %v", err)
	}
	if _, err := h.m.Verify(ctx, manager.InstallRequest{}); !errors.Is(err, manager.ErrDisabled) {
		t.Fatalf("Verify: %v", err)
	}
	if _, err := h.m.Activate(ctx, "x", registry.Confirmations{}); !errors.Is(err, manager.ErrDisabled) {
		t.Fatalf("Activate: %v", err)
	}
	if err := h.m.Delete(ctx, "x"); !errors.Is(err, manager.ErrDisabled) {
		t.Fatalf("Delete: %v", err)
	}
}

func TestInstallActivateLLMEndToEnd(t *testing.T) {
	h := newHarness(t, opts{})
	ex := testutil.StartExample(t, "llm", map[string]string{"API_TOKEN": "x"})

	// Before: the built-in serves the request.
	if r := h.query("before the plugin"); !strings.HasPrefix(r.Reply, "builtin-reply") {
		t.Fatalf("reply = %q", r.Reply)
	}
	h.waitCacheSize(1)

	id := h.installEndpoint(plugins.TypeLLM, "my-llm", ex.URL, func(r *manager.InstallRequest) {
		r.Config = map[string]any{"model": "demo"}
	})
	rec, _ := h.m.Get(id)
	if rec.State != plugins.StateVerified || rec.Verification == nil || !rec.Verification.Passed {
		t.Fatalf("record = %+v", rec)
	}
	if h.llm.calls.Load() != 1 {
		t.Fatal("verification must not touch the built-in LLM")
	}

	res := h.mustActivate(id, registry.Confirmations{})
	if !res.Record.Active || res.Record.State != plugins.StateActive {
		t.Fatalf("after activation: %+v", res.Record)
	}
	if got := h.m.ActiveIDs()[plugins.TypeLLM]; got != id {
		t.Fatalf("active id = %q", got)
	}

	// An uncached request now reaches the plugin.
	r := h.query("a brand new question about the plugin")
	if r.CacheHit || !strings.Contains(r.Reply, "[sdk-llm demo") {
		t.Fatalf("uncached request did not reach the plugin: %+v", r)
	}
	h.waitCacheSize(2)
	// The repeated request is served from the cache, not the plugin.
	r2 := h.query("a brand new question about the plugin")
	if !r2.CacheHit || r2.Reply != r.Reply {
		t.Fatalf("repeat was not a cache hit: %+v", r2)
	}
	// Existing cache entries stay valid across an LLM swap.
	if r := h.query("before the plugin"); !r.CacheHit {
		t.Fatalf("pre-existing entry lost across the swap: %+v", r)
	}
	if h.llm.calls.Load() != 1 {
		t.Fatalf("built-in LLM was called %d times after the swap", h.llm.calls.Load())
	}
}

func TestFailedVerificationNeverActivates(t *testing.T) {
	h := newHarness(t, opts{})
	ex := testutil.StartExample(t, "llm", nil)
	ex.Stop() // endpoint is down
	rec, err := h.m.Install(context.Background(), manager.InstallRequest{Type: "llm", Mode: "endpoint", Name: "down", Endpoint: ex.URL})
	if err != nil {
		t.Fatal(err)
	}
	h.m.WaitIdle()
	got, _ := h.m.Get(rec.ID)
	if got.State != plugins.StateFailed {
		t.Fatalf("state = %s, want failed", got.State)
	}
	if _, err := h.activate(rec.ID, registry.Confirmations{}); err == nil {
		t.Fatal("a plugin that failed verification was activated")
	}
	if h.m.ActiveIDs()[plugins.TypeLLM] != "" {
		t.Fatal("active selection changed")
	}
}

func TestFailedActivationKeepsPreviousPluginActive(t *testing.T) {
	h := newHarness(t, opts{})
	good := testutil.StartExample(t, "llm", nil)
	idGood := h.installEndpoint(plugins.TypeLLM, "good-llm", good.URL, nil)
	h.mustActivate(idGood, registry.Confirmations{})
	if r := h.query("first question"); !strings.Contains(r.Reply, "[sdk-llm") {
		t.Fatalf("good plugin not serving: %+v", r)
	}

	// A second plugin verifies, then dies before activation.
	flaky := testutil.StartExample(t, "llm", nil)
	idFlaky := h.installEndpoint(plugins.TypeLLM, "flaky-llm", flaky.URL, nil)
	flaky.Stop()
	if _, err := h.activate(idFlaky, registry.Confirmations{}); err == nil {
		t.Fatal("activation of a dead plugin succeeded")
	}
	if got := h.m.ActiveIDs()[plugins.TypeLLM]; got != idGood {
		t.Fatalf("active plugin changed to %q after a failed activation", got)
	}
	if r := h.query("second question"); !strings.Contains(r.Reply, "[sdk-llm") {
		t.Fatalf("previous plugin no longer serving: %+v", r)
	}
	failed, _ := h.m.Get(idFlaky)
	if failed.State != plugins.StateFailed || failed.Active {
		t.Fatalf("flaky record = %s active=%v", failed.State, failed.Active)
	}
	stillGood, _ := h.m.Get(idGood)
	if !stillGood.Active || stillGood.State != plugins.StateActive {
		t.Fatalf("good record damaged: %s active=%v", stillGood.State, stillGood.Active)
	}
}

func TestConcurrentActivationIsSerialised(t *testing.T) {
	h := newHarness(t, opts{})
	var ids []string
	for i := 0; i < 4; i++ {
		ex := testutil.StartExample(t, "llm", nil)
		ids = append(ids, h.installEndpoint(plugins.TypeLLM, "llm-"+string(rune('a'+i)), ex.URL, nil))
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok, busy := 0, 0
	for _, id := range ids {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			_, err := h.activate(id, registry.Confirmations{})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				ok++
			case errors.Is(err, manager.ErrBusy):
				busy++
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}(id)
	}
	wg.Wait()
	if ok < 1 || ok+busy != len(ids) {
		t.Fatalf("ok=%d busy=%d", ok, busy)
	}
	// Exactly one plugin is active and memory agrees with the registry.
	active := 0
	for _, id := range ids {
		if r, _ := h.m.Get(id); r.Active {
			active++
			if h.m.ActiveIDs()[plugins.TypeLLM] != id {
				t.Fatal("registry and manager disagree about the active plugin")
			}
		}
	}
	if active != 1 {
		t.Fatalf("%d plugins claim to be active", active)
	}
}

func TestUpgradeRollbackAndDeleteRestrictions(t *testing.T) {
	h := newHarness(t, opts{})
	v1 := testutil.StartExample(t, "llm", nil)
	id1 := h.installEndpoint(plugins.TypeLLM, "up-llm", v1.URL, nil)
	h.mustActivate(id1, registry.Confirmations{})

	v2 := testutil.StartExample(t, "llm", map[string]string{"API_TOKEN": "x"})
	up, err := h.m.Upgrade(context.Background(), id1, manager.InstallRequest{Mode: "endpoint", Endpoint: v2.URL, Version: "1.1.0"})
	if err != nil {
		t.Fatal(err)
	}
	h.waitState(up.ID, plugins.StateVerified)
	if _, err := h.m.Upgrade(context.Background(), id1, manager.InstallRequest{Mode: "endpoint", Endpoint: v2.URL, Version: "1.0.0"}); err == nil {
		t.Fatal("a non-newer version was accepted as an upgrade")
	}
	if r := h.query("q1"); !strings.Contains(r.Reply, "ok]") {
		t.Fatalf("v1 should still serve during the upgrade: %q", r.Reply)
	}

	h.mustActivate(up.ID, registry.Confirmations{})
	if r := h.query("q2 different"); !strings.Contains(r.Reply, "secret-configured") {
		t.Fatalf("v2 not serving after the upgrade: %q", r.Reply)
	}
	cur, _ := h.m.Get(up.ID)
	if cur.PreviousActiveID != id1 {
		t.Fatalf("rollback target = %q, want %q", cur.PreviousActiveID, id1)
	}

	// Delete restrictions: the active plugin and the rollback target are protected.
	if err := h.m.Delete(context.Background(), up.ID); err == nil {
		t.Fatal("deleted the active plugin")
	}
	if err := h.m.Delete(context.Background(), id1); err == nil {
		t.Fatal("deleted the rollback target of the active plugin")
	}

	res, err := h.m.Rollback(context.Background(), up.ID, registry.Confirmations{})
	if err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if res.Record.ID != id1 || !res.Record.Active {
		t.Fatalf("rollback activated %s", res.Record.ID)
	}
	if r := h.query("q3 another new"); strings.Contains(r.Reply, "secret-configured") {
		t.Fatalf("still on v2 after rollback: %q", r.Reply)
	}
	after, _ := h.m.Get(up.ID)
	if after.State != plugins.StateRolledBack || after.Active {
		t.Fatalf("upgraded record = %s active=%v", after.State, after.Active)
	}
	// Now the upgrade is deletable, the active v1 is not.
	if err := h.m.Delete(context.Background(), up.ID); err != nil {
		t.Fatalf("delete of an inactive plugin: %v", err)
	}
	if err := h.m.Delete(context.Background(), id1); err == nil {
		t.Fatal("deleted the active plugin")
	}
}

func TestDeactivateReturnsToBuiltin(t *testing.T) {
	h := newHarness(t, opts{})
	ex := testutil.StartExample(t, "llm", nil)
	id := h.installEndpoint(plugins.TypeLLM, "deact", ex.URL, nil)
	h.mustActivate(id, registry.Confirmations{})
	if _, err := h.m.Deactivate(context.Background(), id, registry.Confirmations{}); err != nil {
		t.Fatal(err)
	}
	if r := h.query("after deactivation"); !strings.HasPrefix(r.Reply, "builtin-reply") {
		t.Fatalf("built-in not restored: %q", r.Reply)
	}
	rec, _ := h.m.Get(id)
	if rec.Active || rec.State != plugins.StateInactive {
		t.Fatalf("record = %s active=%v", rec.State, rec.Active)
	}
	if h.m.ActiveIDs()[plugins.TypeLLM] != "" {
		t.Fatal("slot still points at the plugin")
	}
}

func TestSecretsNeverAppearInViewsLogsOrAudit(t *testing.T) {
	h := newHarness(t, opts{})
	ex := testutil.StartExample(t, "llm", map[string]string{"PLUGIN_AUTH_TOKEN": "super-secret-endpoint-token"})
	const secret = "super-secret-endpoint-token"
	rec, err := h.m.Install(context.Background(), manager.InstallRequest{
		Type: "llm", Mode: "endpoint", Name: "secretive", Endpoint: ex.URL,
		Secrets: map[string]string{manager.EndpointTokenSecret: secret},
	})
	if err != nil {
		t.Fatal(err)
	}
	h.waitState(rec.ID, plugins.StateVerified)
	h.mustActivate(rec.ID, registry.Confirmations{})
	h.query("please use the secret")

	stored, _ := h.reg.Get(rec.ID)
	sealed, _ := json.Marshal(stored.Secrets)
	if strings.Contains(string(sealed), secret) {
		t.Fatal("secret stored in plaintext")
	}
	if len(stored.Secrets) != 1 {
		t.Fatalf("stored secrets = %v", stored.Secrets)
	}
	view, _ := json.Marshal(manager.ViewOf(stored))
	blob := string(view)
	for _, l := range h.logs(rec.ID) {
		blob += l
	}
	events, _ := h.reg.Audit("", 100)
	ev, _ := json.Marshal(events)
	blob += string(ev)
	if strings.Contains(blob, secret) {
		t.Fatalf("secret leaked into a view, log or audit event")
	}
	for _, forbidden := range []string{stored.Secrets[manager.EndpointTokenSecret].Ciphertext, stored.Secrets[manager.EndpointTokenSecret].Nonce} {
		if strings.Contains(blob, forbidden) {
			t.Fatal("encrypted secret blob leaked into a view")
		}
	}
}

func TestEndpointRejectsSSRFTargets(t *testing.T) {
	h := newHarness(t, opts{cfg: func(c *manager.Config) { c.AllowInsecureEndpoints = false }})
	for _, ep := range []string{"http://plugin.example.com", "https://127.0.0.1", "https://169.254.169.254", "https://localhost:8080", "https://10.0.0.1", "https://user:pw@plugin.example.com", "https://plugin.example.com/?token=abc"} {
		if _, err := h.m.Install(context.Background(), manager.InstallRequest{Type: "llm", Mode: "endpoint", Name: "ssrf", Endpoint: ep}); err == nil {
			t.Errorf("endpoint %q accepted", ep)
		}
	}
	events, _ := h.reg.Audit("", 50)
	if len(events) == 0 {
		t.Error("denied installs must be audited")
	}
}

func TestInstallValidation(t *testing.T) {
	h := newHarness(t, opts{})
	ctx := context.Background()
	for name, req := range map[string]manager.InstallRequest{
		"unknown type":        {Type: "gpu", Mode: "endpoint", Endpoint: "http://127.0.0.1:1"},
		"unknown mode":        {Type: "llm", Mode: "ftp"},
		"bad config key":      {Type: "llm", Mode: "endpoint", Name: "x", Endpoint: "http://127.0.0.1:1", Config: map[string]any{"nope": 1}},
		"foreign secret":      {Type: "llm", Mode: "endpoint", Name: "x", Endpoint: "http://127.0.0.1:1", Secrets: map[string]string{"OTHER": "v"}},
		"empty secret":        {Type: "llm", Mode: "endpoint", Name: "x", Endpoint: "http://127.0.0.1:1", Secrets: map[string]string{manager.EndpointTokenSecret: ""}},
		"image without ctl":   {Type: "llm", Mode: "image", Image: "example/x:1"},
		"repo without ctl":    {Type: "llm", Mode: "repository", Repo: "https://github.com/a/b"},
		"manifest wrong type": {Type: "llm", Mode: "endpoint", Endpoint: "http://127.0.0.1:1", Manifest: "apiVersion: llmcache.dev/v1alpha1\nkind: Plugin\nmetadata: {name: p, version: 1.0.0}\nspec: {type: queue}\n"},
		"bad manifest":        {Type: "llm", Mode: "endpoint", Endpoint: "http://127.0.0.1:1", Manifest: "apiVersion: v2\n"},
		"bad name":            {Type: "llm", Mode: "endpoint", Name: "Bad Name!", Endpoint: "http://127.0.0.1:1"},
	} {
		if _, err := h.m.Install(ctx, req); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	all, _ := h.m.List()
	if len(all) != 0 {
		t.Fatalf("%d records created by rejected requests", len(all))
	}
}
