// Package contract holds the reusable contract-test suites, one per plugin
// type. They are ordinary Go (not _test files) so the same checks run in three
// places: `go test` against the SDK examples, the plugin manager before it
// activates a candidate, and the llm-cache-plugin verify command.
//
// A suite drives a plugin only through the versioned protocol client, exactly as
// the orchestrator will. Suites for stateful stores (vector store, persistence)
// write to the plugin and flush it, so they must only be pointed at a candidate
// that is not serving real traffic.
package contract

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/leenkabha/llm_cache/internal/plugins"
	"github.com/leenkabha/llm_cache/internal/plugins/protocol"
	"github.com/leenkabha/llm_cache/internal/plugins/secrets"
)

// Target is what a suite runs against.
type Target struct {
	Type   plugins.Type
	Client *protocol.Client

	Model             string // llm: model name to send
	VectorDim         int    // vector stores: dimension hint when /health does not report one
	AllowUnnormalized bool   // embedders: skip the unit-length check
	// ExpectedBackend is the registry name a Python plugin must have registered
	// and selected (embedding-model, vector-index, similarity-metric).
	ExpectedBackend string
	// RequireEmpty makes destructive suites refuse to run against a non-empty
	// store instead of flushing it.
	RequireEmpty bool
}

// Result is one check's outcome.
type Result struct {
	Name     string        `json:"name"`
	Status   string        `json:"status"` // pass | fail | skip
	Detail   string        `json:"detail,omitempty"`
	Duration time.Duration `json:"duration_ns"`
}

// Report is a suite's complete outcome.
type Report struct {
	Type    plugins.Type `json:"type"`
	Results []Result     `json:"results"`
}

// Passed is true when no check failed.
func (r Report) Passed() bool {
	for _, x := range r.Results {
		if x.Status == "fail" {
			return false
		}
	}
	return len(r.Results) > 0
}

// Failures lists failed checks.
func (r Report) Failures() []Result {
	var out []Result
	for _, x := range r.Results {
		if x.Status == "fail" {
			out = append(out, x)
		}
	}
	return out
}

// Summary is a one-line human summary.
func (r Report) Summary() string {
	pass, fail, skip := 0, 0, 0
	for _, x := range r.Results {
		switch x.Status {
		case "pass":
			pass++
		case "fail":
			fail++
		default:
			skip++
		}
	}
	s := fmt.Sprintf("%s contract: %d passed, %d failed, %d skipped", r.Type, pass, fail, skip)
	if fail > 0 {
		var names []string
		for _, f := range r.Failures() {
			names = append(names, f.Name)
		}
		s += " (failed: " + strings.Join(names, ", ") + ")"
	}
	return s
}

// Run executes the suite for t.Type.
func Run(ctx context.Context, t Target) Report {
	rep := Report{Type: t.Type}
	r := &runner{ctx: ctx, rep: &rep}
	switch t.Type {
	case plugins.TypeLLM:
		runLLM(r, t)
	case plugins.TypeEmbedder:
		runEmbedder(r, t)
	case plugins.TypeVectorStore:
		runVectorStore(r, t)
	case plugins.TypePersistence:
		runPersistence(r, t)
	case plugins.TypeQueue:
		runQueue(r, t)
	case plugins.TypePolicy:
		runPolicy(r, t)
	case plugins.TypeEmbeddingModel:
		runEmbeddingModel(r, t)
	case plugins.TypeVectorIndex:
		runVectorIndex(r, t)
	case plugins.TypeSimilarityMetric:
		runSimilarityMetric(r, t)
	default:
		rep.Results = append(rep.Results, Result{Name: "type", Status: "fail", Detail: fmt.Sprintf("no contract suite for %q", t.Type)})
	}
	return rep
}

// errSkip marks a check that could not run (with the reason).
type errSkip string

func (e errSkip) Error() string { return string(e) }

const caseTimeout = 45 * time.Second

type runner struct {
	ctx context.Context
	rep *Report
	mu  sync.Mutex
}

// check runs one named case with a timeout, recovering panics.
func (r *runner) check(name string, fn func(ctx context.Context) error) {
	start := time.Now()
	res := Result{Name: name, Status: "pass"}
	if err := r.ctx.Err(); err != nil {
		res.Status, res.Detail = "fail", "verification canceled: "+err.Error()
	} else {
		ctx, cancel := context.WithTimeout(r.ctx, caseTimeout)
		func() {
			defer func() {
				if p := recover(); p != nil {
					res.Status, res.Detail = "fail", fmt.Sprintf("panic: %v", p)
				}
			}()
			if err := fn(ctx); err != nil {
				var sk errSkip
				if errors.As(err, &sk) {
					res.Status, res.Detail = "skip", string(sk)
				} else {
					res.Status, res.Detail = "fail", secrets.Scrub(err.Error())
				}
			}
		}()
		cancel()
	}
	res.Duration = time.Since(start)
	r.mu.Lock()
	r.rep.Results = append(r.rep.Results, res)
	r.mu.Unlock()
}

// parallel runs fn n times concurrently and returns the first error.
func parallel(n int, fn func(i int) error) error {
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = fn(i)
		}(i)
	}
	wg.Wait()
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}

// expect4xx sends a request the plugin must refuse as a client error (not 5xx,
// not 2xx).
func expect4xx(ctx context.Context, c *protocol.Client, method, path string, body any) error {
	status, err := c.Do(ctx, method, path, body, nil)
	if err == nil {
		return fmt.Errorf("%s %s accepted an invalid request (HTTP %d)", method, path, status)
	}
	var se *protocol.StatusError
	if !errors.As(err, &se) {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	if se.Status < 400 || se.Status > 499 {
		return fmt.Errorf("%s %s answered an invalid request with HTTP %d, want 4xx", method, path, se.Status)
	}
	return nil
}

func statusOf(err error) int {
	var se *protocol.StatusError
	if errors.As(err, &se) {
		return se.Status
	}
	return 0
}
