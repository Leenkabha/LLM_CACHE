package testutil

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/leenkabha/llm_cache/internal/plugins/ctlapi"
	"github.com/leenkabha/llm_cache/internal/plugins/registry"
)

// FakeController implements the manager's Controller interface without Docker:
// "images" are SDK examples, and "starting an instance" launches the example as
// a local process with the environment the real controller would inject.
type FakeController struct {
	T *testing.T

	mu        sync.Mutex
	Prepared  []ctlapi.PrepareRequest
	Started   []ctlapi.StartRequest
	Stopped   []string
	running   map[string]*Example
	Examples  map[string]string // image reference or repository URL -> SDK example name
	FailWith  error             // Prepare returns this
	ScanState string            // "" = skipped, or "failed"
}

func NewFakeController(t *testing.T) *FakeController {
	return &FakeController{T: t, running: map[string]*Example{}, Examples: map[string]string{}}
}

func digestOf(s string) string {
	h := sha256.Sum256([]byte(s))
	return "sha256:" + hex.EncodeToString(h[:])
}

// Register maps an image or repository to an SDK example.
func (f *FakeController) Register(ref, example string) {
	f.mu.Lock()
	f.Examples[ref] = example
	f.mu.Unlock()
}

func (f *FakeController) Prepare(_ context.Context, req ctlapi.PrepareRequest) (*ctlapi.PrepareResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Prepared = append(f.Prepared, req)
	if f.FailWith != nil {
		return nil, f.FailWith
	}
	ref := req.Image
	if ref == "" {
		ref = req.Repo
	}
	example, ok := f.Examples[ref]
	if !ok {
		return nil, fmt.Errorf("could not pull %s", ref)
	}
	text := req.Manifest
	if text == "" || req.Mode == "repository" {
		b, err := os.ReadFile(filepath.Join(RepoRoot(), "sdk", "examples", example, "plugin.yaml"))
		if err != nil {
			return nil, err
		}
		text = string(b)
	}
	res := &ctlapi.PrepareResult{Manifest: text}
	if req.Mode == "repository" {
		res.Commit = digestOf(req.Repo + req.Revision)[7:47]
	}
	if req.DryRun {
		return res, nil
	}
	res.Digest = digestOf(ref + example)
	res.ImageSize = 1 << 20
	scan := &registry.ScanReport{Scanner: "none", Status: "skipped", Message: "no scanner"}
	if f.ScanState == "failed" {
		scan = &registry.ScanReport{Scanner: "fake", Status: "failed", Message: "2 CRITICAL vulnerabilities"}
	}
	res.Scan = scan
	res.Provenance = &registry.Provenance{Builder: "fake", Commit: res.Commit, ImageSize: res.ImageSize}
	res.Log = []string{"prepared " + ref}
	return res, nil
}

func (f *FakeController) exampleForDigest(d string) (string, bool) {
	for ref, ex := range f.Examples {
		if digestOf(ref+ex) == d {
			return ex, true
		}
	}
	return "", false
}

func (f *FakeController) Start(_ context.Context, req ctlapi.StartRequest) (*ctlapi.Instance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Started = append(f.Started, req)
	if ex, ok := f.running[req.InstanceID]; ok {
		return &ctlapi.Instance{ID: req.InstanceID, BaseURL: ex.URL}, nil
	}
	name, ok := f.exampleForDigest(req.Digest)
	if !ok {
		return nil, errors.New("unknown image digest")
	}
	ex := StartExample(f.T, name, req.Env)
	f.running[req.InstanceID] = ex
	return &ctlapi.Instance{ID: req.InstanceID, BaseURL: ex.URL}, nil
}

func (f *FakeController) Stop(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Stopped = append(f.Stopped, id)
	if ex, ok := f.running[id]; ok {
		ex.Stop()
		delete(f.running, id)
	}
	return nil
}

func (f *FakeController) Logs(context.Context, string, int) ([]string, error) { return nil, nil }
func (f *FakeController) Health(context.Context) error                        { return nil }

// Running reports whether an instance is currently up.
func (f *FakeController) Running(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.running[id]
	return ok
}

// Wait polls until cond holds or the timeout passes.
func Wait(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return cond()
}
