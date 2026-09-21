package contract

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"

	"github.com/leenkabha/llm_cache/internal/plugins/protocol"
)

func norm(v []float64) float64 {
	var s float64
	for _, x := range v {
		s += x * x
	}
	return math.Sqrt(s)
}

func cosineDistance(a, b []float64) float64 {
	var dot float64
	for i := range a {
		dot += a[i] * b[i]
	}
	na, nb := norm(a), norm(b)
	if na == 0 || nb == 0 {
		return 1
	}
	return 1 - dot/(na*nb)
}

func runEmbedder(r *runner, t Target) {
	e := protocol.NewEmbedder(t.Client)
	texts := []string{"hello", "What is virtual memory?", "héllo wörld 你好", "a", "the quick brown fox jumps over the lazy dog"}

	r.check("health", func(ctx context.Context) error { return e.Health(ctx) })
	var dim int
	var name string
	r.check("model_info", func(ctx context.Context) error {
		info, err := e.ModelInfo(ctx)
		if err != nil {
			return err
		}
		dim, name = info.Dim, info.Name
		return nil
	})
	r.check("vector_shape_and_finite", func(ctx context.Context) error {
		for _, tx := range texts {
			v, model, err := e.EmbedDetailed(ctx, tx) // validates length == dim and finiteness
			if err != nil {
				return fmt.Errorf("%q: %w", tx, err)
			}
			if dim != 0 && len(v) != dim {
				return fmt.Errorf("vector length %d differs from model-info dim %d", len(v), dim)
			}
			if model != "" && name != "" && model != name {
				return fmt.Errorf("embed reported model %q but model-info says %q", model, name)
			}
		}
		return nil
	})
	r.check("stable_dimension", func(ctx context.Context) error {
		first := -1
		for _, tx := range texts {
			v, err := e.Embed(ctx, tx)
			if err != nil {
				return err
			}
			if first < 0 {
				first = len(v)
			} else if len(v) != first {
				return fmt.Errorf("dimension changed from %d to %d", first, len(v))
			}
		}
		return nil
	})
	r.check("normalized_unit_length", func(ctx context.Context) error {
		if t.AllowUnnormalized {
			return errSkip("unit length not required for this plugin")
		}
		for _, tx := range texts {
			v, err := e.Embed(ctx, tx)
			if err != nil {
				return err
			}
			if n := norm(v); math.Abs(n-1) > 1e-3 {
				return fmt.Errorf("vector for %q has length %.5f, want 1 (cosine similarity assumes unit vectors)", tx, n)
			}
		}
		return nil
	})
	r.check("deterministic_for_same_text", func(ctx context.Context) error {
		a, err := e.Embed(ctx, "What is virtual memory?")
		if err != nil {
			return err
		}
		b, err := e.Embed(ctx, "What is virtual memory?")
		if err != nil {
			return err
		}
		if d := cosineDistance(a, b); d > 1e-3 {
			return fmt.Errorf("the same text embedded twice differs by distance %.5f; identical prompts would miss the cache", d)
		}
		return nil
	})
	r.check("concurrent_calls", func(ctx context.Context) error {
		ref, err := e.Embed(ctx, "concurrent")
		if err != nil {
			return err
		}
		return parallel(8, func(int) error {
			v, err := e.Embed(ctx, "concurrent")
			if err != nil {
				return err
			}
			if d := cosineDistance(ref, v); d > 1e-3 {
				return errors.New("concurrent calls returned different vectors for the same text")
			}
			return nil
		})
	})
	r.check("malformed_request_rejected", func(ctx context.Context) error {
		return expect4xx(ctx, t.Client, http.MethodPost, "/v1/embed", map[string]any{"text": 42})
	})
}
