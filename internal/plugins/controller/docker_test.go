package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/leenkabha/llm_cache/internal/plugins"
	"github.com/leenkabha/llm_cache/internal/plugins/contract"
	"github.com/leenkabha/llm_cache/internal/plugins/ctlapi"
	"github.com/leenkabha/llm_cache/internal/plugins/protocol"
	"github.com/leenkabha/llm_cache/internal/plugins/testutil"
)

// These tests drive a real Docker daemon: they build images, start hardened
// containers and run the contract suites through the controller's proxy. They
// are skipped unless LLMCACHE_DOCKER_TESTS=1 (and Docker works), because they
// pull base images and take minutes.
func dockerSetup(t *testing.T) (*ctlapi.Client, *Server, *DockerCLI) {
	t.Helper()
	if os.Getenv("LLMCACHE_DOCKER_TESTS") != "1" {
		t.Skip("set LLMCACHE_DOCKER_TESTS=1 to run the Docker integration tests")
	}
	rt := NewDockerCLI()
	pctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := rt.Ping(pctx); err != nil {
		t.Skipf("docker is not available: %v", err)
	}
	s, err := New(Config{
		Token: "docker-test-controller-token", Publish: true, AllowLocalImages: true, AllowLocalGit: true,
		RunnerDir: testutil.RepoRoot(), BuildTimeout: 12 * time.Minute, HealthWait: 60 * time.Second,
	}, rt, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(func() { srv.Close(); s.Close() })
	return ctlapi.NewClient(srv.URL, "docker-test-controller-token"), s, rt
}

func docker(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command("docker", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("docker %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func dockerTry(args ...string) (string, error) {
	out, err := exec.Command("docker", args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func uid() string { return fmt.Sprintf("it-%06d", rand.Intn(1_000_000)) }

// through builds a protocol client that reaches an instance via the proxy.
func through(t *testing.T, c *ctlapi.Client, inst *ctlapi.Instance) *protocol.Client {
	t.Helper()
	pc, err := protocol.NewClient(inst.BaseURL, &http.Client{Timeout: 30 * time.Second}, c.Token)
	if err != nil {
		t.Fatal(err)
	}
	return pc
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
		t.Fatal(rep.Summary())
	}
	t.Log(rep.Summary())
}

func manifestOf(t *testing.T, dir string) string {
	b, err := os.ReadFile(filepath.Join(dir, "plugin.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestDockerImageModeEndToEnd(t *testing.T) {
	c, _, _ := dockerSetup(t)
	ctx := context.Background()
	src := filepath.Join(testutil.RepoRoot(), "sdk", "examples", "llm")
	tag := "llmcache-it-llm:" + uid()
	docker(t, "build", "-q", "-t", tag, src)
	t.Cleanup(func() { _, _ = dockerTry("image", "rm", "-f", tag) })

	res, err := c.Prepare(ctx, ctlapi.PrepareRequest{Mode: plugins.ModeImage, Type: plugins.TypeLLM, Image: tag, Manifest: manifestOf(t, src)})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if !strings.HasPrefix(res.Digest, "sha256:") || res.ImageSize <= 0 || res.Scan == nil || res.Scan.Status != "skipped" {
		t.Fatalf("result = %+v", res)
	}
	t.Logf("pinned %s (%d bytes), scan=%s", res.Digest, res.ImageSize, res.Scan.Status)

	id := uid()
	t.Cleanup(func() { _ = c.Stop(ctx, id) })
	const secret = "runtime-only-secret-value-123"
	inst, err := c.Start(ctx, ctlapi.StartRequest{InstanceID: id, Type: plugins.TypeLLM, Digest: res.Digest, Manifest: res.Manifest,
		Env: map[string]string{"API_TOKEN": secret, "CONFIG_MODEL": "demo"}})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	name := containerName(id)

	// The container really is hardened.
	var ins []struct {
		HostConfig struct {
			Privileged     bool
			ReadonlyRootfs bool
			CapDrop        []string
			CapAdd         []string
			SecurityOpt    []string
			PidsLimit      int64
			Memory         int64
			NanoCpus       int64
			NetworkMode    string
			PidMode        string
			Binds          []string
			Devices        []any
		}
		Config struct {
			User   string
			Labels map[string]string
		}
	}
	if err := json.Unmarshal([]byte(docker(t, "inspect", name)), &ins); err != nil || len(ins) != 1 {
		t.Fatalf("inspect: %v", err)
	}
	hc := ins[0].HostConfig
	if hc.Privileged || !hc.ReadonlyRootfs || len(hc.CapAdd) != 0 || hc.PidMode != "" || len(hc.Binds) != 0 || len(hc.Devices) != 0 || hc.NetworkMode == "host" {
		t.Errorf("isolation violated: %+v", hc)
	}
	if len(hc.CapDrop) != 1 || !strings.EqualFold(hc.CapDrop[0], "ALL") {
		t.Errorf("capabilities not dropped: %v", hc.CapDrop)
	}
	if !strings.Contains(strings.Join(hc.SecurityOpt, " "), "no-new-privileges") || hc.PidsLimit <= 0 || hc.Memory <= 0 || hc.NanoCpus <= 0 {
		t.Errorf("limits missing: %+v", hc)
	}
	if ins[0].Config.User != "10001:10001" {
		t.Errorf("user = %q", ins[0].Config.User)
	}
	if strings.Contains(fmt.Sprint(ins[0].Config.Labels), secret) {
		t.Error("a secret appears in container labels")
	}
	// Behaviourally: non-root, read-only root, writable /tmp.
	if out := docker(t, "exec", name, "id", "-u"); out != "10001" {
		t.Errorf("runs as uid %s", out)
	}
	if out, err := dockerTry("exec", name, "sh", "-c", "touch /pwned"); err == nil {
		t.Errorf("the root filesystem is writable: %s", out)
	}
	if out, err := dockerTry("exec", name, "sh", "-c", "touch /tmp/ok"); err != nil {
		t.Errorf("/tmp is not writable: %s", out)
	}
	// The secret is not in the image layers.
	if hist := docker(t, "history", "--no-trunc", tag); strings.Contains(hist, secret) {
		t.Error("the secret is in the image history")
	}
	// The plugin refuses unauthenticated calls (published port, direct).
	addr, err := (&DockerCLI{Bin: "docker", Env: os.Environ()}).Address(ctx, name, 8080)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post("http://"+addr+"/v1/complete", "application/json", strings.NewReader(`{"model":"m","prompt":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Errorf("direct unauthenticated call = %d, want 401", resp.StatusCode)
	}

	// The contract suite passes through the controller's proxy.
	pc := through(t, c, inst)
	report(t, contract.Run(ctx, contract.Target{Type: plugins.TypeLLM, Client: pc, Model: "demo"}))
	// The injected secret reached the process (the example reports that it is set, never its value).
	got, err := protocol.NewLLM(pc, "demo").Complete(ctx, "check the secret")
	if err != nil || !strings.Contains(got, "secret-configured") || strings.Contains(got, secret) {
		t.Errorf("reply = %q, %v", got, err)
	}
	logs, _ := c.Logs(ctx, id, 50)
	if strings.Contains(strings.Join(logs, "\n"), secret) {
		t.Error("the secret appears in the plugin logs")
	}

	// Stop removes the container and its network.
	if err := c.Stop(ctx, id); err != nil {
		t.Fatal(err)
	}
	if out, err := dockerTry("inspect", name); err == nil {
		t.Errorf("container still exists: %s", out)
	}
	if out, err := dockerTry("network", "inspect", name); err == nil {
		t.Errorf("network still exists: %s", out)
	}
}

// exampleRepo copies an SDK example into a fresh git repository.
func exampleRepo(t *testing.T, example string) (string, string) {
	src := filepath.Join(testutil.RepoRoot(), "sdk", "examples", example)
	files := map[string]string{}
	_ = filepath.Walk(src, func(p string, i os.FileInfo, err error) error {
		if err == nil && i.Mode().IsRegular() {
			rel, _ := filepath.Rel(src, p)
			b, _ := os.ReadFile(p)
			files[filepath.ToSlash(rel)] = string(b)
		}
		return nil
	})
	return gitRepo(t, files)
}

func TestDockerRepositoryModeBuildsAndVerifiesTheQueueExample(t *testing.T) {
	c, _, _ := dockerSetup(t)
	ctx := context.Background()
	repo, commit := exampleRepo(t, "queue")
	id := uid()
	res, err := c.Prepare(ctx, ctlapi.PrepareRequest{Mode: plugins.ModeRepository, Type: plugins.TypeQueue, Repo: repo, Revision: "main", PluginID: id})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	t.Cleanup(func() { _, _ = dockerTry("image", "rm", "-f", res.Digest) })
	if res.Commit != commit || res.Provenance == nil || res.Provenance.Commit != commit || res.Provenance.SourceHash == "" || res.Provenance.Builder != "docker build" {
		t.Fatalf("provenance = %+v commit=%s", res.Provenance, res.Commit)
	}
	t.Cleanup(func() { _ = c.Stop(ctx, id) })
	inst, err := c.Start(ctx, ctlapi.StartRequest{InstanceID: id, Type: plugins.TypeQueue, Digest: res.Digest, Manifest: res.Manifest})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	report(t, contract.Run(ctx, contract.Target{Type: plugins.TypeQueue, Client: through(t, c, inst)}))
}

// The three Python plugin types are built into specialised runner images from the
// platform's own services plus the developer's package, then verified.
func TestDockerPythonRunners(t *testing.T) {
	c, _, _ := dockerSetup(t)
	ctx := context.Background()
	cases := []struct {
		example string
		typ     plugins.Type
		backend string
		env     map[string]string
	}{
		{"embedding-model", plugins.TypeEmbeddingModel, "sdk-hash", map[string]string{"CONFIG_DIM": "64"}},
		{"vector-index", plugins.TypeVectorIndex, "sdk-numpy", map[string]string{"VECTOR_DIM": "8"}},
		{"similarity-metric", plugins.TypeSimilarityMetric, "sdk-angular", map[string]string{"VECTOR_DIM": "8"}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(string(tc.typ), func(t *testing.T) {
			repo, _ := exampleRepo(t, tc.example)
			id := uid()
			res, err := c.Prepare(ctx, ctlapi.PrepareRequest{Mode: plugins.ModeRepository, Type: tc.typ, Repo: repo, PluginID: id})
			if err != nil {
				t.Fatalf("prepare: %v", err)
			}
			t.Cleanup(func() { _, _ = dockerTry("image", "rm", "-f", res.Digest) })
			t.Cleanup(func() { _ = c.Stop(ctx, id) })
			inst, err := c.Start(ctx, ctlapi.StartRequest{InstanceID: id, Type: tc.typ, Digest: res.Digest, Manifest: res.Manifest, Env: tc.env})
			if err != nil {
				t.Fatalf("start: %v", err)
			}
			t.Cleanup(func() {
				if t.Failed() {
					logs, _ := c.Logs(ctx, id, 40)
					t.Logf("runner logs:\n%s", strings.Join(logs, "\n"))
				}
			})
			report(t, contract.Run(ctx, contract.Target{Type: tc.typ, Client: through(t, c, inst), ExpectedBackend: tc.backend, RequireEmpty: true}))
			// The runner image was built without the heavy built-in model.
			if out := docker(t, "exec", containerName(id), "python", "-c", "import importlib.util as u; print(u.find_spec('sentence_transformers') is None)"); out != "True" {
				t.Errorf("runner image unexpectedly contains sentence-transformers: %s", out)
			}
		})
	}
}
