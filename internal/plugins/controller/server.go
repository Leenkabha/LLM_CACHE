// Package controller is the internal plugin-controller service. It is the only
// component that talks to the container runtime, so it is the only one that
// needs (restricted) Docker access; the orchestrator and the public web service
// never receive it.
//
// Responsibilities: fetch exact source revisions, build in isolation, pull and
// inspect images, pin digests, scan, start hardened instances with runtime-only
// secrets, health-check them, proxy the orchestrator's calls to them, and keep
// sanitised logs. It authenticates every request with a shared token and is not
// meant to be reachable from outside the private network.
package controller

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/leenkabha/llm_cache/internal/plugins"
	"github.com/leenkabha/llm_cache/internal/plugins/ctlapi"
	"github.com/leenkabha/llm_cache/internal/plugins/manifest"
	"github.com/leenkabha/llm_cache/internal/plugins/registry"
	"github.com/leenkabha/llm_cache/internal/plugins/secrets"
)

// Config configures the controller.
type Config struct {
	Token string // required shared secret with the orchestrator

	RunnerDir string // directory holding embedding_service/ and vector_store_service/ for Python plugin builds

	BuildTimeout   time.Duration
	MaxImageBytes  int64
	MaxSourceBytes int64
	MaxSourceFiles int
	CPULimit       float64 // default and ceiling per plugin
	MemoryLimit    int64   // default and ceiling per plugin (bytes)

	AllowLocalImages bool // use images already present locally instead of always pulling (development)
	AllowLocalGit    bool // accept absolute local repository paths (development and tests)
	AllowEgress      bool // honour spec.network.egress: internet (otherwise plugins never get a route out)
	RequireScan      bool // treat "no scanner" as a failure

	// Reach selects how the controller connects to plugin containers:
	//   publish - each instance's port is published on 127.0.0.1 (controller on the host; development)
	//   network - the controller joins each plugin's private network (controller in a container)
	Publish        bool
	SelfContainer  string // this container's name/ID, for network mode
	ProxyTimeout   time.Duration
	MaxProxyBody   int64
	MaxProxyResult int64
	HealthWait     time.Duration
}

func (c *Config) defaults() {
	if c.BuildTimeout <= 0 {
		c.BuildTimeout = 10 * time.Minute
	}
	if c.MaxImageBytes <= 0 {
		c.MaxImageBytes = 2 << 30
	}
	if c.MaxSourceBytes <= 0 {
		c.MaxSourceBytes = 100 << 20
	}
	if c.MaxSourceFiles <= 0 {
		c.MaxSourceFiles = 20000
	}
	if c.CPULimit <= 0 {
		c.CPULimit = 1
	}
	if c.MemoryLimit <= 0 {
		c.MemoryLimit = 512 << 20
	}
	if c.ProxyTimeout <= 0 {
		c.ProxyTimeout = 120 * time.Second
	}
	if c.MaxProxyBody <= 0 {
		c.MaxProxyBody = 64 << 20
	}
	if c.MaxProxyResult <= 0 {
		c.MaxProxyResult = 32 << 20
	}
	if c.HealthWait <= 0 {
		c.HealthWait = 60 * time.Second
	}
}

// Server is the controller.
type Server struct {
	cfg     Config
	rt      Runtime
	scan    Scanner
	tmpRoot string

	mu        sync.Mutex
	instances map[string]*instance
	starting  map[string]*sync.Mutex // per-instance start serialisation

	proxyClient *http.Client
}

type instance struct {
	name, addr, authToken, healthPath string
	port                              int
}

var instanceIDRE = regexp.MustCompile(`^[a-z0-9_-]{1,64}$`)

// New builds a controller.
func New(cfg Config, rt Runtime, scanner Scanner) (*Server, error) {
	cfg.defaults()
	if len(cfg.Token) < 16 {
		return nil, errors.New("PLUGIN_CONTROLLER_TOKEN must be set to at least 16 characters")
	}
	if scanner == nil {
		scanner = NoScanner{}
	}
	tmp, err := os.MkdirTemp("", "plugin-controller-*")
	if err != nil {
		return nil, err
	}
	return &Server{
		cfg: cfg, rt: rt, scan: scanner, tmpRoot: tmp,
		instances: map[string]*instance{}, starting: map[string]*sync.Mutex{},
		proxyClient: &http.Client{
			Timeout:       cfg.ProxyTimeout,
			Transport:     &http.Transport{Proxy: nil, MaxIdleConnsPerHost: 16, DisableCompression: true},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

// Close removes scratch space.
func (s *Server) Close() { _ = os.RemoveAll(s.tmpRoot) }

// Handler returns the HTTP API.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 200, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("POST /v1/prepare", s.auth(s.handlePrepare))
	mux.HandleFunc("POST /v1/instances", s.auth(s.handleStart))
	mux.HandleFunc("DELETE /v1/instances/{id}", s.auth(s.handleStop))
	mux.HandleFunc("GET /v1/instances/{id}/logs", s.auth(s.handleLogs))
	mux.HandleFunc("/v1/proxy/{id}/{path...}", s.auth(s.handleProxy))
	return mux
}

func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || subtle.ConstantTimeCompare([]byte(got), []byte(s.cfg.Token)) != 1 {
			writeErr(w, 401, "controller token required")
			return
		}
		next(w, r)
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": secrets.Scrub(msg)})
}

func decodeBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<20)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeErr(w, 400, "invalid request body")
		return false
	}
	return true
}

// ---- prepare -----------------------------------------------------------------

type buildLog struct{ lines []string }

func (b *buildLog) add(format string, a ...any) {
	b.lines = append(b.lines, secrets.Scrub(fmt.Sprintf(format, a...)))
	if len(b.lines) > 400 {
		b.lines = b.lines[len(b.lines)-400:]
	}
}

func (s *Server) handlePrepare(w http.ResponseWriter, r *http.Request) {
	var req ctlapi.PrepareRequest
	if !decodeBody(w, r, &req) {
		return
	}
	res, err := s.Prepare(r.Context(), req)
	if err != nil {
		code := 422
		var pe *PrepareError
		if errors.As(err, &pe) {
			code = pe.Status
		}
		writeErr(w, code, err.Error())
		return
	}
	writeJSON(w, 200, res)
}

// PrepareError is a client-visible preparation failure.
type PrepareError struct {
	Status int
	Msg    string
}

func (e *PrepareError) Error() string { return e.Msg }

func bad(format string, a ...any) error {
	return &PrepareError{Status: 422, Msg: fmt.Sprintf(format, a...)}
}

// Prepare makes a plugin runnable: image or repository in, pinned digest, scan
// report, provenance and canonical manifest out.
func (s *Server) Prepare(ctx context.Context, req ctlapi.PrepareRequest) (*ctlapi.PrepareResult, error) {
	typ, err := plugins.ParseType(string(req.Type))
	if err != nil {
		return nil, bad("%v", err)
	}
	ctx, cancel := context.WithTimeout(ctx, s.cfg.BuildTimeout)
	defer cancel()
	log := &buildLog{}
	var res *ctlapi.PrepareResult
	switch req.Mode {
	case plugins.ModeImage:
		res, err = s.prepareImage(ctx, req, typ, log)
	case plugins.ModeRepository:
		res, err = s.prepareRepo(ctx, req, typ, log)
	default:
		return nil, bad("mode must be image or repository")
	}
	if res != nil {
		res.Log = log.lines
	}
	if err != nil {
		return nil, err
	}
	return res, nil
}

func (s *Server) parseManifest(text string, typ plugins.Type) (*manifest.Manifest, error) {
	m, err := manifest.Parse([]byte(text))
	if err != nil {
		return nil, bad("%v", err)
	}
	if m.Spec.Type != string(typ) {
		return nil, bad("manifest is for type %s but %s was requested", m.Spec.Type, typ)
	}
	if m.Spec.Runtime.Mode == manifest.RuntimeEndpoint {
		return nil, bad("manifest runtime.mode is endpoint; use the hosted-endpoint install mode")
	}
	return m, nil
}

func (s *Server) finishImage(ctx context.Context, info ImageInfo, mf *manifest.Manifest, log *buildLog, prov *registry.Provenance) (*ctlapi.PrepareResult, error) {
	if info.Size > s.cfg.MaxImageBytes {
		s.rt.RemoveImage(context.Background(), info.ID)
		return nil, bad("image is %d bytes; the limit is %d", info.Size, s.cfg.MaxImageBytes)
	}
	log.add("image %s (%d bytes)", info.Pinned(), info.Size)
	rep, err := s.scan.Scan(ctx, info.ID)
	if err != nil {
		return nil, bad("image scan could not run: %v", err)
	}
	if rep.Status == "skipped" && s.cfg.RequireScan {
		return nil, bad("no vulnerability scanner is configured but PLUGIN_REQUIRE_SCAN is set")
	}
	log.add("scan (%s): %s %s", rep.Scanner, rep.Status, rep.Message)
	if rep.Status == "failed" {
		return nil, bad("image scan failed: %s", rep.Message)
	}
	text, _ := yaml.Marshal(mf)
	sum := sha256.Sum256(text)
	prov.ManifestSHA = hex.EncodeToString(sum[:])
	prov.ImageSize = info.Size
	return &ctlapi.PrepareResult{Digest: info.Pinned(), ImageSize: info.Size, Manifest: string(text), Scan: rep, Provenance: prov}, nil
}

func (s *Server) prepareImage(ctx context.Context, req ctlapi.PrepareRequest, typ plugins.Type, log *buildLog) (*ctlapi.PrepareResult, error) {
	if !imageRefRE.MatchString(req.Image) || strings.HasPrefix(req.Image, "-") {
		return nil, bad("invalid image reference")
	}
	log.add("resolving image %s", req.Image)
	info, err := s.rt.PullOrInspect(ctx, req.Image, s.cfg.AllowLocalImages)
	if err != nil {
		return nil, bad("could not pull the image: %v", err)
	}
	text := req.Manifest
	if text == "" {
		data, err := s.rt.ReadFileFromImage(ctx, info.ID, "/plugin.yaml", manifest.MaxBytes)
		if err != nil {
			return nil, bad("no manifest supplied and the image has no /plugin.yaml: %v", err)
		}
		text = string(data)
		log.add("manifest read from /plugin.yaml in the image (the image was not started)")
	}
	mf, err := s.parseManifest(text, typ)
	if err != nil {
		return nil, err
	}
	if _, _, _, _, err := mf.Effective(s.cfg.CPULimit, s.cfg.MemoryLimit, s.cfg.CPULimit, s.cfg.MemoryLimit); err != nil {
		return nil, bad("%v", err)
	}
	if req.DryRun {
		t, _ := yaml.Marshal(mf)
		return &ctlapi.PrepareResult{Digest: info.Pinned(), ImageSize: info.Size, Manifest: string(t)}, nil
	}
	return s.finishImage(ctx, info, mf, log, &registry.Provenance{Builder: "registry-pull", BuiltAt: time.Now().UTC()})
}

func (s *Server) prepareRepo(ctx context.Context, req ctlapi.PrepareRequest, typ plugins.Type, log *buildLog) (*ctlapi.PrepareResult, error) {
	work, err := os.MkdirTemp(s.tmpRoot, "build-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(work)
	src := filepath.Join(work, "src")

	log.add("fetching %s at %s", secrets.SafeURL(req.Repo), orHEAD(req.Revision))
	commit, err := FetchRepo(ctx, req.Repo, req.Revision, src, s.cfg.AllowLocalGit)
	if err != nil {
		return nil, bad("could not fetch the repository: %v", err)
	}
	log.add("resolved to commit %s", commit)
	return s.prepareTree(ctx, req, typ, log, work, src, commit, secrets.SafeURL(req.Repo))
}

// prepareTree validates and builds an already checked-out source tree. It is the
// shared tail of repository installs and of local verification (llm-cache-plugin
// verify), so both exercise exactly the same build path.
func (s *Server) prepareTree(ctx context.Context, req ctlapi.PrepareRequest, typ plugins.Type, log *buildLog, work, src, commit, origin string) (*ctlapi.PrepareResult, error) {
	var err error
	lim := Limits{MaxSourceBytes: s.cfg.MaxSourceBytes, MaxFiles: s.cfg.MaxSourceFiles}
	srcHash, err := CheckTree(src, lim)
	if err != nil {
		return nil, bad("%v", err)
	}
	data, err := ReadRegular(src, "plugin.yaml", manifest.MaxBytes)
	if err != nil {
		return nil, bad("the repository has no valid plugin.yaml at its root: %v", err)
	}
	mf, err := s.parseManifest(string(data), typ)
	if err != nil {
		return nil, err
	}
	if _, _, _, _, err := mf.Effective(s.cfg.CPULimit, s.cfg.MemoryLimit, s.cfg.CPULimit, s.cfg.MemoryLimit); err != nil {
		return nil, bad("%v", err)
	}
	text, _ := yaml.Marshal(mf)
	if req.DryRun {
		return &ctlapi.PrepareResult{Manifest: string(text), Commit: commit}, nil
	}

	// Choose the build context and Dockerfile.
	var contextDir, dockerfile string
	if mf.Spec.Runtime.Mode == manifest.RuntimePython {
		if s.cfg.RunnerDir == "" {
			return nil, bad("this controller has no runner sources (PLUGIN_RUNNER_DIR) and cannot build Python plugins")
		}
		contextDir = filepath.Join(work, "pyctx")
		if err := os.MkdirAll(contextDir, 0o755); err != nil {
			return nil, err
		}
		if dockerfile, err = BuildPythonContext(s.cfg.RunnerDir, mf, src, contextDir, lim); err != nil {
			return nil, bad("%v", err)
		}
		log.add("generated a runner build for python plugin %s (backend %s)", mf.Spec.Python.Module, mf.Spec.Python.Backend)
	} else {
		if contextDir, err = SafeJoin(src, mf.Spec.Runtime.Context); err != nil {
			return nil, bad("runtime.context: %v", err)
		}
		if !isDir(contextDir) {
			return nil, bad("runtime.context %q is not a directory in the repository", mf.Spec.Runtime.Context)
		}
		dfPath, err := SafeJoin(src, mf.Spec.Runtime.Dockerfile)
		if err != nil {
			return nil, bad("runtime.dockerfile: %v", err)
		}
		if !isFile(dfPath) {
			return nil, bad("runtime.dockerfile %q is not a file in the repository", mf.Spec.Runtime.Dockerfile)
		}
		if dockerfile, err = filepath.Rel(contextDir, dfPath); err != nil || strings.HasPrefix(dockerfile, "..") {
			return nil, bad("runtime.dockerfile must be inside runtime.context")
		}
	}

	tag := fmt.Sprintf("llmcache-plugin-build/%s:%s", orDefault(req.PluginID, "x"), commit[:12])
	tag = strings.ToLower(tag)
	log.add("building %s (no build arguments, no secrets)", tag)
	blog, err := s.rt.Build(ctx, BuildSpec{
		ContextDir: contextDir, Dockerfile: filepath.Join(contextDir, dockerfile), Tag: tag,
		Labels: map[string]string{"llmcache.source": origin, "llmcache.commit": commit},
	})
	for _, l := range blog {
		log.add("build: %s", l)
	}
	if err != nil {
		return nil, bad("%v", err)
	}
	info, err := s.rt.InspectImage(ctx, tag)
	if err != nil {
		return nil, bad("could not inspect the built image: %v", err)
	}
	prov := &registry.Provenance{Builder: "docker build", RepoURL: origin, Commit: commit, SourceHash: srcHash, BuiltAt: time.Now().UTC()}
	res, err := s.finishImage(ctx, info, mf, log, prov)
	if err != nil {
		s.rt.RemoveImage(context.Background(), info.ID)
		return nil, err
	}
	res.Commit = commit
	return res, nil
}

func orHEAD(r string) string { return orDefault(r, "HEAD") }
func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// ---- instances ---------------------------------------------------------------

func containerName(id string) string { return "llmcache-plugin-" + id }

// pluginToken is derived, not stored: the same instance always gets the same
// bearer token, so a restarted controller can keep proxying without state.
func (s *Server) pluginToken(id string) string {
	mac := hmac.New(sha256.New, []byte(s.cfg.Token))
	mac.Write([]byte("plugin-auth:" + id))
	return hex.EncodeToString(mac.Sum(nil))[:48]
}

func envHash(digest, manifestText string, env map[string]string) string {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	fmt.Fprintf(h, "%s\n%s\n", digest, manifestText)
	for _, k := range keys {
		fmt.Fprintf(h, "%s=%s\n", k, env[k])
	}
	return hex.EncodeToString(h.Sum(nil))
}

func (s *Server) handleStart(w http.ResponseWriter, r *http.Request) {
	var req ctlapi.StartRequest
	if !decodeBody(w, r, &req) {
		return
	}
	inst, err := s.Start(r.Context(), req)
	if err != nil {
		code := 422
		var pe *PrepareError
		if errors.As(err, &pe) {
			code = pe.Status
		}
		writeErr(w, code, err.Error())
		return
	}
	writeJSON(w, 200, ctlapi.Instance{ID: inst})
}

// Start runs (or re-uses) an isolated instance and waits until it is healthy.
func (s *Server) Start(ctx context.Context, req ctlapi.StartRequest) (string, error) {
	if !instanceIDRE.MatchString(req.InstanceID) {
		return "", bad("invalid instance id")
	}
	if !regexp.MustCompile(`^sha256:[a-f0-9]{64}$`).MatchString(req.Digest) {
		return "", bad("digest must be an immutable sha256 reference")
	}
	typ, err := plugins.ParseType(string(req.Type))
	if err != nil {
		return "", bad("%v", err)
	}
	mf, err := s.parseManifest(req.Manifest, typ)
	if err != nil {
		return "", err
	}
	cpu, mem, pids, tmpfs, err := mf.Effective(s.cfg.CPULimit, s.cfg.MemoryLimit, s.cfg.CPULimit, s.cfg.MemoryLimit)
	if err != nil {
		return "", bad("%v", err)
	}
	egress := mf.Spec.Network.Egress == manifest.EgressInternet
	if egress && !s.cfg.AllowEgress {
		return "", bad("the plugin asks for internet egress but this installation does not allow it (PLUGIN_ALLOW_EGRESS)")
	}

	port := mf.Spec.Runtime.Port
	env := map[string]string{}
	if mf.Spec.Runtime.Mode == manifest.RuntimePython {
		port = RunnerPort(typ)
		for k, v := range RunnerEnv(mf, req.Env["CONFIG_METRIC"]) {
			env[k] = v
		}
	}
	for k, v := range req.Env {
		if _, taken := env[k]; !taken {
			env[k] = v
		}
	}
	env["PORT"] = strconv.Itoa(port)
	env["PLUGIN_AUTH_TOKEN"] = s.pluginToken(req.InstanceID)
	// Native math libraries (OpenMP in FAISS, BLAS in numpy) start one thread per
	// HOST core by default, which exceeds a container's process limit and its CPU
	// share. Size their pools to the CPU limit unless the plugin says otherwise.
	threads := strconv.Itoa(max(1, int(cpu)))
	for _, k := range []string{"OMP_NUM_THREADS", "OPENBLAS_NUM_THREADS", "MKL_NUM_THREADS"} {
		if _, set := env[k]; !set {
			env[k] = threads
		}
	}

	s.mu.Lock()
	lk := s.starting[req.InstanceID]
	if lk == nil {
		lk = &sync.Mutex{}
		s.starting[req.InstanceID] = lk
	}
	s.mu.Unlock()
	lk.Lock()
	defer lk.Unlock()

	name := containerName(req.InstanceID)
	hash := envHash(req.Digest, req.Manifest, env)
	healthWait := req.HealthWait
	if healthWait <= 0 || healthWait > 5*time.Minute {
		healthWait = s.cfg.HealthWait
	}

	if ci, err := s.rt.Inspect(ctx, name); err != nil {
		return "", err
	} else if ci != nil {
		if ci.Running && ci.Labels[LabelEnvHash] == hash {
			if addr, err := s.address(ctx, name, port); err == nil {
				if s.waitHealthy(ctx, addr, mf.Spec.Health.Path, 10*time.Second) == nil {
					s.remember(req.InstanceID, name, addr, port, mf.Spec.Health.Path)
					return req.InstanceID, nil
				}
			}
		}
		s.rt.Remove(ctx, name) // different image/config, stopped, or unhealthy: recreate
	}

	spec := RunSpec{
		Name: name, Image: req.Digest, Port: port, Env: env, CPUs: cpu, Memory: mem, PIDs: pids, Tmpfs: tmpfs,
		Network: name, Internal: !egress && !s.cfg.Publish, Publish: s.cfg.Publish, AttachSelf: s.cfg.SelfContainer,
		Labels: map[string]string{LabelManaged: "true", LabelID: req.InstanceID, LabelEnvHash: hash, LabelDigest: req.Digest},
	}
	if !s.cfg.Publish && s.cfg.SelfContainer == "" {
		return "", bad("the controller cannot reach plugin networks: set PLUGIN_CONTROLLER_PUBLISH=true (controller on the host) or PLUGIN_CONTROLLER_CONTAINER (controller in a container)")
	}
	if err := s.rt.Run(ctx, spec); err != nil {
		s.rt.Remove(context.Background(), name)
		return "", bad("%v", err)
	}
	addr, err := s.address(ctx, name, port)
	if err != nil {
		s.rt.Remove(context.Background(), name)
		return "", bad("%v", err)
	}
	if err := s.waitHealthy(ctx, addr, mf.Spec.Health.Path, healthWait); err != nil {
		logs, _ := s.rt.Logs(context.Background(), name, 30)
		s.rt.Remove(context.Background(), name)
		return "", bad("the plugin did not become healthy: %v; last log lines: %s", err, strings.Join(logs, " | "))
	}
	s.remember(req.InstanceID, name, addr, port, mf.Spec.Health.Path)
	return req.InstanceID, nil
}

func (s *Server) address(ctx context.Context, name string, port int) (string, error) {
	if s.cfg.Publish {
		return s.rt.Address(ctx, name, port)
	}
	return fmt.Sprintf("%s:%d", name, port), nil
}

func (s *Server) remember(id, name, addr string, port int, health string) {
	s.mu.Lock()
	s.instances[id] = &instance{name: name, addr: addr, port: port, authToken: s.pluginToken(id), healthPath: health}
	s.mu.Unlock()
}

func (s *Server) waitHealthy(ctx context.Context, addr, path string, wait time.Duration) error {
	deadline := time.Now().Add(wait)
	hc := &http.Client{Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	var last error
	for {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+path, nil)
		resp, err := hc.Do(req)
		if err == nil {
			io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return nil
			}
			last = fmt.Errorf("health returned HTTP %d", resp.StatusCode)
		} else {
			last = errors.New("health endpoint unreachable")
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return last
		}
		select {
		case <-time.After(300 * time.Millisecond):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !instanceIDRE.MatchString(id) {
		writeErr(w, 400, "invalid instance id")
		return
	}
	s.mu.Lock()
	delete(s.instances, id)
	s.mu.Unlock()
	s.rt.Remove(r.Context(), containerName(id))
	writeJSON(w, 200, map[string]string{"status": "stopped"})
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !instanceIDRE.MatchString(id) {
		writeErr(w, 400, "invalid instance id")
		return
	}
	n, _ := strconv.Atoi(r.URL.Query().Get("tail"))
	if n < 1 || n > 500 {
		n = 100
	}
	lines, err := s.rt.Logs(r.Context(), containerName(id), n)
	if err != nil {
		writeErr(w, 404, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"lines": lines})
}

// ---- proxy -------------------------------------------------------------------

func (s *Server) lookup(ctx context.Context, id string, refresh bool) (*instance, error) {
	s.mu.Lock()
	in := s.instances[id]
	s.mu.Unlock()
	if in != nil && !refresh {
		return in, nil
	}
	// Unknown (controller restarted) or stale address: rebuild from the runtime.
	name := containerName(id)
	ci, err := s.rt.Inspect(ctx, name)
	if err != nil || ci == nil || !ci.Running {
		return nil, errors.New("no such running instance")
	}
	port := 0
	if in != nil {
		port = in.port
	}
	if port == 0 {
		return nil, errors.New("instance is unknown to this controller; start it again")
	}
	addr, err := s.address(ctx, name, port)
	if err != nil {
		return nil, err
	}
	health := "/health"
	if in != nil {
		health = in.healthPath
	}
	s.remember(id, name, addr, port, health)
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.instances[id], nil
}

func (s *Server) handleProxy(w http.ResponseWriter, r *http.Request) {
	id, path := r.PathValue("id"), r.PathValue("path")
	if !instanceIDRE.MatchString(id) {
		writeErr(w, 400, "invalid instance id")
		return
	}
	// Only the plugin protocol is reachable: nothing else the container serves.
	if path != "health" && !strings.HasPrefix(path, "v1/") || strings.Contains(path, "..") || strings.Contains(path, "//") {
		writeErr(w, 404, "not a plugin protocol path")
		return
	}
	in, err := s.lookup(r.Context(), id, false)
	if err != nil {
		writeErr(w, 404, err.Error())
		return
	}
	body := http.MaxBytesReader(w, r.Body, s.cfg.MaxProxyBody)
	do := func(target *instance) (*http.Response, error) {
		u := "http://" + target.addr + "/" + path
		if r.URL.RawQuery != "" {
			u += "?" + r.URL.RawQuery
		}
		req, err := http.NewRequestWithContext(r.Context(), r.Method, u, body)
		if err != nil {
			return nil, err
		}
		if ct := r.Header.Get("Content-Type"); ct != "" {
			req.Header.Set("Content-Type", ct)
		}
		req.Header.Set("Accept", "application/json")
		// The orchestrator's credential stops here; the plugin gets its own.
		req.Header.Set("Authorization", "Bearer "+target.authToken)
		return s.proxyClient.Do(req)
	}
	resp, err := do(in)
	if err != nil && r.Method == http.MethodGet { // stale published port after a container restart
		if in2, lerr := s.lookup(r.Context(), id, true); lerr == nil {
			resp, err = do(in2)
		}
	}
	if err != nil {
		writeErr(w, 502, "plugin unreachable")
		return
	}
	defer resp.Body.Close()
	if resp.ContentLength > s.cfg.MaxProxyResult {
		writeErr(w, 502, "plugin response exceeds the size limit")
		return
	}
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(resp.StatusCode)
	n, err := io.Copy(w, io.LimitReader(resp.Body, s.cfg.MaxProxyResult+1))
	if err == nil && n > s.cfg.MaxProxyResult {
		panic(http.ErrAbortHandler) // truncated on purpose: the caller sees a broken response, never a partial success
	}
}

// Recover re-learns running instances after a controller restart.
func (s *Server) Recover(ctx context.Context) { log.Printf("controller ready") }
