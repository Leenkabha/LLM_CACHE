package manifest

import (
	"strings"
	"testing"
)

const validLLM = `
apiVersion: llmcache.dev/v1alpha1
kind: Plugin
metadata:
  name: my-plugin
  version: 1.0.0
  description: Example LLM Cache plugin
spec:
  type: llm
  runtime:
    mode: container
    dockerfile: Dockerfile
    port: 8080
  health:
    path: /health
    timeout: 5s
  config:
    properties:
      model:
        type: string
        required: true
      request_timeout:
        type: duration
        default: 30s
  secrets:
    - name: API_TOKEN
      required: true
`

func TestParseValid(t *testing.T) {
	m, err := Parse([]byte(validLLM))
	if err != nil {
		t.Fatal(err)
	}
	if m.Metadata.Name != "my-plugin" || m.Spec.Type != "llm" || m.Spec.Contract.Version != "v1" {
		t.Fatalf("unexpected manifest %+v", m)
	}
	if m.Spec.Network.Egress != EgressNone {
		t.Fatalf("egress default = %q, want none", m.Spec.Network.Egress)
	}
}

func TestParseAllNineTypes(t *testing.T) {
	for _, typ := range []string{"llm", "embedder", "vector-store", "persistence", "queue", "policy"} {
		src := "apiVersion: llmcache.dev/v1alpha1\nkind: Plugin\nmetadata: {name: p, version: 0.1.0}\nspec:\n  type: " + typ + "\n"
		if _, err := Parse([]byte(src)); err != nil {
			t.Errorf("%s: %v", typ, err)
		}
	}
	for _, typ := range []string{"embedding-model", "vector-index", "similarity-metric"} {
		src := "apiVersion: llmcache.dev/v1alpha1\nkind: Plugin\nmetadata: {name: p, version: 0.1.0}\nspec:\n  type: " + typ + "\n  runtime: {mode: python}\n  python: {module: my_pkg, backend: my-thing}\n"
		if _, err := Parse([]byte(src)); err != nil {
			t.Errorf("%s: %v", typ, err)
		}
	}
}

func TestParseRejects(t *testing.T) {
	sub := func(old, new string) string { return strings.Replace(validLLM, old, new, 1) }
	cases := []struct {
		name, src, want string
	}{
		{"empty", "", "empty"},
		{"not yaml", "a: [", "not valid YAML"},
		{"bad apiVersion", sub("v1alpha1", "v2"), "unsupported apiVersion"},
		{"bad kind", sub("kind: Plugin", "kind: Widget"), "kind must be"},
		{"unknown type", sub("type: llm", "type: gpu"), "unknown plugin type"},
		{"bad name upper", sub("name: my-plugin", "name: My_Plugin"), "metadata.name"},
		{"bad name dash", sub("name: my-plugin", "name: -x"), "metadata.name"},
		{"bad semver", sub("version: 1.0.0", "version: 1.0"), "semantic version"},
		{"bad contract", sub("spec:\n  type: llm", "spec:\n  type: llm\n  contract:\n    version: v9"), "contract version"},
		{"unknown field", sub("port: 8080", "port: 8080\n    surprise: 1"), "surprise"},
		{"privileged", sub("port: 8080", "port: 8080\n    privileged: true"), "privileged containers are not allowed"},
		{"host network", sub("port: 8080", "port: 8080\n    hostNetwork: true"), "host networking"},
		{"host network snake", sub("port: 8080", "port: 8080\n    host_network: true"), "host networking"},
		{"host pid", sub("port: 8080", "port: 8080\n    hostPID: true"), "PID namespace"},
		{"volumes", sub("port: 8080", "port: 8080\n    volumes: [\"/:/host\"]"), "host filesystem mounts"},
		{"docker socket", sub("port: 8080", "port: 8080\n    dockerSocket: true"), "Docker socket"},
		{"capabilities", sub("port: 8080", "port: 8080\n    cap_add: [SYS_ADMIN]"), "capabilities"},
		{"command", sub("port: 8080", "port: 8080\n    command: [sh]"), "custom commands"},
		{"build args", sub("port: 8080", "port: 8080\n    buildArgs: {A: b}"), "build arguments"},
		{"absolute dockerfile", sub("dockerfile: Dockerfile", "dockerfile: /etc/passwd"), "relative"},
		{"dotdot dockerfile", sub("dockerfile: Dockerfile", "dockerfile: ../x/Dockerfile"), "no '..'"},
		{"backslash context", sub("port: 8080", "port: 8080\n    context: 'a\\\\b'"), "invalid characters"},
		{"privileged port", sub("port: 8080", "port: 80"), "1024"},
		{"bad health path", sub("path: /health", "path: http://evil/x"), "health.path"},
		{"health query", sub("path: /health", "path: /health?x=1"), "health.path"},
		{"health timeout", sub("timeout: 5s", "timeout: 10h"), "health.timeout"},
		{"secret in default", sub("default: 30s", "default: sk-abcdefghijklmnopqrstuvwxyz"), "credential"},
		{"secret-like property", sub("request_timeout:", "api_token:"), "looks like a secret"},
		{"secret value field", sub("- name: API_TOKEN", "- name: API_TOKEN\n      value: hunter2"), "value"},
		{"secret bad name", sub("API_TOKEN", "api token"), "environment-variable"},
		{"secret reserved", sub("API_TOKEN", "LD_PRELOAD"), "reserved"},
		{"cpu too big", sub("spec:\n  type: llm", "spec:\n  type: llm\n  resources:\n    cpu: '64'"), "maximum"},
		{"memory too big", sub("spec:\n  type: llm", "spec:\n  type: llm\n  resources:\n    memory: 64Gi"), "between 16Mi and 4Gi"},
		{"memory junk", sub("spec:\n  type: llm", "spec:\n  type: llm\n  resources:\n    memory: lots"), "invalid size"},
		{"pids", sub("spec:\n  type: llm", "spec:\n  type: llm\n  resources:\n    pids: 100000"), "pids"},
		{"egress", sub("spec:\n  type: llm", "spec:\n  type: llm\n  network:\n    egress: all"), "egress"},
		{"python type as container", strings.Replace(validLLM, "type: llm", "type: embedding-model", 1), "Python registry plugin"},
		{"python mode for llm", sub("mode: container", "mode: python"), "only valid for"},
		{"bad property type", sub("type: string", "type: blob"), "unsupported type"},
		{"default type mismatch", sub("default: 30s", "default: 12"), "duration"},
		{"two documents", validLLM + "\n---\nfoo: bar\n", "exactly one"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Parse([]byte(c.src))
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error %q does not mention %q", err, c.want)
			}
		})
	}
}

func TestParseRejectsOversized(t *testing.T) {
	if _, err := Parse([]byte(strings.Repeat("#", MaxBytes+1))); err == nil {
		t.Fatal("oversized manifest accepted")
	}
}

func TestPropertyNamedLikeDangerousKeyIsAllowed(t *testing.T) {
	src := strings.Replace(validLLM, "request_timeout:", "user:", 1)
	if _, err := Parse([]byte(src)); err != nil {
		t.Fatalf("a developer-chosen property called 'user' should be fine: %v", err)
	}
}

func TestResolveConfig(t *testing.T) {
	m, _ := Parse([]byte(validLLM))
	cfg, err := m.ResolveConfig(map[string]any{"model": "demo"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg["model"] != "demo" || cfg["request_timeout"] != "30s" {
		t.Fatalf("cfg = %v", cfg)
	}
	for name, in := range map[string]map[string]any{
		"missing required": {},
		"unknown key":      {"model": "x", "nope": 1},
		"wrong type":       {"model": 3},
		"bad duration":     {"model": "x", "request_timeout": "soon"},
	} {
		if _, err := m.ResolveConfig(in); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestCheckSecrets(t *testing.T) {
	m, _ := Parse([]byte(validLLM))
	if err := m.CheckSecrets([]string{"API_TOKEN"}); err != nil {
		t.Fatal(err)
	}
	if err := m.CheckSecrets(nil); err == nil {
		t.Fatal("missing required secret accepted")
	}
	if err := m.CheckSecrets([]string{"API_TOKEN", "OTHER"}); err == nil {
		t.Fatal("undeclared secret accepted")
	}
}

func TestEffectiveResourcesHonourOperatorCeiling(t *testing.T) {
	src := strings.Replace(validLLM, "spec:\n  type: llm", "spec:\n  type: llm\n  resources:\n    cpu: 2\n    memory: 1Gi", 1)
	m, err := Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, err := m.Effective(0.5, 256<<20, 1, 512<<20); err == nil {
		t.Fatal("request above the operator ceiling accepted")
	}
	cpu, mem, _, _, err := m.Effective(0.5, 256<<20, 4, 2<<30)
	if err != nil || cpu != 2 || mem != 1<<30 {
		t.Fatalf("cpu=%v mem=%v err=%v", cpu, mem, err)
	}
}

func TestParseQuantities(t *testing.T) {
	for in, want := range map[string]float64{"500m": 0.5, "0.25": 0.25, "2": 2} {
		if got, err := ParseCPU(in); err != nil || got != want {
			t.Errorf("ParseCPU(%q) = %v, %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "0", "-1", "abc", "1m0"} {
		if _, err := ParseCPU(bad); err == nil {
			t.Errorf("ParseCPU(%q) accepted", bad)
		}
	}
	if b, err := ParseBytes("256Mi"); err != nil || b != 256<<20 {
		t.Errorf("ParseBytes = %v, %v", b, err)
	}
	for _, bad := range []string{"", "0", "-5Mi", "Mi", "1.5Gi"} {
		if _, err := ParseBytes(bad); err == nil {
			t.Errorf("ParseBytes(%q) accepted", bad)
		}
	}
}
