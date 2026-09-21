package setup

import (
	"strings"
	"testing"

	"github.com/leenkabha/llm_cache/internal/config"
	"github.com/leenkabha/llm_cache/internal/orchestrator"
	"github.com/leenkabha/llm_cache/internal/persistence"
	"github.com/leenkabha/llm_cache/internal/plugins/testutil"
	"github.com/leenkabha/llm_cache/internal/policy"
)

func deps(t *testing.T) orchestrator.Dependencies {
	t.Helper()
	pol, _ := policy.NewManager("lru")
	return orchestrator.Dependencies{
		Embedder: testutil.NewHashEmbedder(4), VectorStore: testutil.NewMemVectorStore(4),
		Store: persistence.NewMemoryStore(), Queue: &testutil.MemQueue{}, Policy: pol,
	}
}

func TestDisabledByDefaultLeavesDependenciesUntouched(t *testing.T) {
	d := deps(t)
	p, err := Prepare(config.Config{}, d)
	if err != nil {
		t.Fatal(err)
	}
	if p.Manager != nil {
		t.Fatal("a manager exists although installation is disabled")
	}
	if p.Deps.Embedder != d.Embedder || p.Deps.Store != d.Store || p.Deps.Policy != d.Policy {
		t.Fatal("dependencies were wrapped while the platform is disabled")
	}
}

func TestEnablingRequiresAdminTokenAndValidKey(t *testing.T) {
	good := "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="
	cases := map[string]struct {
		cfg  config.Config
		want string
	}{
		"no admin token":           {config.Config{EnablePluginInstallation: true, PluginSecretKey: good, PluginRegistryBackend: "memory"}, "ADMIN_TOKEN"},
		"no key":                   {config.Config{EnablePluginInstallation: true, AdminToken: "t", PluginRegistryBackend: "memory"}, "PLUGIN_SECRET_KEY"},
		"short key":                {config.Config{EnablePluginInstallation: true, AdminToken: "t", PluginSecretKey: "c2hvcnQ=", PluginRegistryBackend: "memory"}, "32 bytes"},
		"controller without token": {config.Config{EnablePluginInstallation: true, AdminToken: "t", PluginSecretKey: good, PluginRegistryBackend: "memory", PluginControllerURL: "http://c:1"}, "PLUGIN_CONTROLLER_TOKEN"},
		"bad timeout":              {config.Config{EnablePluginInstallation: true, AdminToken: "t", PluginSecretKey: good, PluginRegistryBackend: "memory", PluginBuildTimeout: "soon"}, "PLUGIN_BUILD_TIMEOUT"},
		"bad memory":               {config.Config{EnablePluginInstallation: true, AdminToken: "t", PluginSecretKey: good, PluginRegistryBackend: "memory", PluginMemoryLimit: "lots"}, "PLUGIN_MEMORY_LIMIT"},
		"bad registry":             {config.Config{EnablePluginInstallation: true, AdminToken: "t", PluginSecretKey: good, PluginRegistryBackend: "etcd"}, "PLUGIN_REGISTRY_BACKEND"},
	}
	for name, c := range cases {
		if _, err := Prepare(c.cfg, deps(t)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want mention of %q", name, err, c.want)
		}
	}
	p, err := Prepare(config.Config{EnablePluginInstallation: true, AdminToken: "t", PluginSecretKey: good, PluginRegistryBackend: "memory",
		PluginCPULimit: "500m", PluginMemoryLimit: "256Mi"}, deps(t))
	if err != nil || p.Manager == nil {
		t.Fatalf("valid configuration rejected: %v", err)
	}
	if p.Deps.Embedder == nil {
		t.Fatal("no dependencies returned")
	}
}
