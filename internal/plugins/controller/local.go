package controller

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/leenkabha/llm_cache/internal/plugins"
	"github.com/leenkabha/llm_cache/internal/plugins/contract"
	"github.com/leenkabha/llm_cache/internal/plugins/ctlapi"
	"github.com/leenkabha/llm_cache/internal/plugins/manifest"
	"github.com/leenkabha/llm_cache/internal/plugins/protocol"
	"github.com/leenkabha/llm_cache/internal/plugins/secrets"
)

// LocalOptions configures local verification of a plugin directory.
type LocalOptions struct {
	Dir       string            // directory containing plugin.yaml
	RunnerDir string            // LLM_CACHE checkout (for Python plugins)
	Env       map[string]string // extra configuration, e.g. CONFIG_DIM
	Keep      bool              // leave the container and image behind for debugging
	Timeout   time.Duration
}

// FindRunnerDir looks for a directory containing the platform's services,
// starting from start and walking up.
func FindRunnerDir(start string) string {
	if v := os.Getenv("LLM_CACHE_RUNNER_DIR"); v != "" {
		return v
	}
	dir, _ := filepath.Abs(start)
	for i := 0; i < 8; i++ {
		if isDir(filepath.Join(dir, "embedding_service", "app")) && isDir(filepath.Join(dir, "vector_store_service", "app")) {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return ""
}

// LocalVerify builds the plugin in Dir exactly as the controller would, starts
// it isolated on this machine, runs its contract suite, and cleans up. It needs
// Docker and (for Python plugins) a checkout of LLM_CACHE for the runner sources.
func LocalVerify(ctx context.Context, o LocalOptions, progress func(string)) (*manifest.Manifest, *contract.Report, error) {
	if progress == nil {
		progress = func(string) {}
	}
	data, err := ReadRegular(o.Dir, "plugin.yaml", manifest.MaxBytes)
	if err != nil {
		return nil, nil, fmt.Errorf("plugin.yaml: %w", err)
	}
	mf, err := manifest.Parse(data)
	if err != nil {
		return nil, nil, err
	}
	typ, _ := plugins.ParseType(mf.Spec.Type)
	if mf.Spec.Runtime.Mode == manifest.RuntimeEndpoint {
		return mf, nil, errors.New("this manifest describes a hosted endpoint; use --endpoint")
	}
	if o.Timeout <= 0 {
		o.Timeout = 15 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()

	rt := NewDockerCLI()
	if err := rt.Ping(ctx); err != nil {
		return mf, nil, fmt.Errorf("docker is required to verify a container plugin locally: %w", err)
	}
	tok := make([]byte, 16)
	_, _ = rand.Read(tok)
	if o.RunnerDir == "" && typ.IsPython() {
		o.RunnerDir = FindRunnerDir(".")
		if o.RunnerDir == "" {
			return mf, nil, errors.New("Python plugins are built into the platform's runner: run this from a checkout of LLM_CACHE or pass --runner-dir")
		}
	}
	s, err := New(Config{Token: hex.EncodeToString(tok), Publish: true, AllowLocalImages: true, RunnerDir: o.RunnerDir, BuildTimeout: o.Timeout, HealthWait: 90 * time.Second}, rt, NoScanner{})
	if err != nil {
		return mf, nil, err
	}
	defer s.Close()

	work, err := os.MkdirTemp(s.tmpRoot, "local-*")
	if err != nil {
		return mf, nil, err
	}
	src := filepath.Join(work, "src")
	if err := copyTree(o.Dir, src, Limits{MaxSourceBytes: s.cfg.MaxSourceBytes, MaxFiles: s.cfg.MaxSourceFiles}); err != nil {
		return mf, nil, fmt.Errorf("copy plugin source: %w", err)
	}
	blog := &buildLog{}
	progress("building the plugin image (this pulls base images the first time)...")
	res, err := s.prepareTree(ctx, ctlapi.PrepareRequest{Mode: plugins.ModeRepository, Type: typ, PluginID: "local"}, typ, blog, work, src, strings.Repeat("0", 40), "local")
	for _, l := range blog.lines {
		progress("  " + l)
	}
	if err != nil {
		return mf, nil, err
	}
	if !o.Keep {
		defer rt.RemoveImage(context.Background(), res.Digest)
	}

	env := map[string]string{}
	cfg, err := mf.ResolveConfig(nil)
	if err != nil {
		// required config without a default: the developer must supply it
		if len(o.Env) == 0 {
			return mf, nil, fmt.Errorf("the manifest has required configuration; pass values with --config key=value (%v)", err)
		}
	}
	for k, v := range cfg {
		env["CONFIG_"+strings.ToUpper(k)] = fmt.Sprint(v)
	}
	for k, v := range o.Env {
		env[k] = v
	}
	if typ == plugins.TypeVectorIndex || typ == plugins.TypeSimilarityMetric {
		if _, ok := env["VECTOR_DIM"]; !ok {
			dim := mf.Spec.Verify.VectorDim
			if dim == 0 {
				dim = 8
			}
			env["VECTOR_DIM"] = fmt.Sprint(dim)
		}
	}
	id := "local-" + hex.EncodeToString(tok[:4])
	progress("starting the plugin in an isolated container (read-only, non-root, no privileges)...")
	if _, err := s.Start(ctx, ctlapi.StartRequest{InstanceID: id, Type: typ, Digest: res.Digest, Manifest: res.Manifest, Env: env}); err != nil {
		return mf, nil, err
	}
	if !o.Keep {
		defer s.rt.Remove(context.Background(), containerName(id))
	} else {
		progress("container " + containerName(id) + " left running (--keep)")
	}
	addr, err := s.address(ctx, containerName(id), portOf(mf, typ))
	if err != nil {
		return mf, nil, err
	}
	client, err := protocol.NewClient("http://"+addr, &http.Client{Timeout: 60 * time.Second}, s.pluginToken(id))
	if err != nil {
		return mf, nil, err
	}
	t := contract.Target{Type: typ, Client: client, Model: "verify", VectorDim: mf.Spec.Verify.VectorDim, RequireEmpty: true}
	if mf.Spec.Python != nil {
		t.ExpectedBackend = mf.Spec.Python.Backend
	}
	progress("running the " + string(typ) + " contract suite...")
	rep := contract.Run(ctx, t)
	for i := range rep.Results {
		rep.Results[i].Detail = secrets.Scrub(rep.Results[i].Detail)
	}
	return mf, &rep, nil
}

func portOf(mf *manifest.Manifest, typ plugins.Type) int {
	if mf.Spec.Runtime.Mode == manifest.RuntimePython {
		return RunnerPort(typ)
	}
	return mf.Spec.Runtime.Port
}
