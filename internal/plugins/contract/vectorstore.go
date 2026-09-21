package contract

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sort"
	"time"

	"github.com/leenkabha/llm_cache/internal/plugins/protocol"
	"github.com/leenkabha/llm_cache/internal/vectorstore"
)

// unit returns the i-th basis vector of the given dimension.
func unit(dim, i int) []float64 {
	v := make([]float64, dim)
	v[i%dim] = 1
	return v
}

func normalize(v []float64) []float64 {
	n := norm(v)
	out := make([]float64, len(v))
	for i := range v {
		out[i] = v[i] / n
	}
	return out
}

// mix returns normalize(e0 + w*e1).
func mix(dim int, w float64) []float64 {
	v := make([]float64, dim)
	v[0] = 1
	v[1] = w
	return normalize(v)
}

const huge = 1e9

// vectorStoreDim finds the dimension to test with.
func vectorStoreDim(ctx context.Context, vs *protocol.VectorStore, t Target) int {
	if info, err := vs.Info(ctx); err == nil && info.Dim >= 2 {
		return info.Dim
	}
	if t.VectorDim >= 2 {
		return t.VectorDim
	}
	return 4
}

func runVectorStore(r *runner, t Target) {
	vs := protocol.NewVectorStore(t.Client)
	var dim int

	r.check("health", func(ctx context.Context) error { return vs.Health(ctx) })
	r.check("dimension", func(ctx context.Context) error {
		dim = vectorStoreDim(ctx, vs, t)
		return nil
	})
	if dim == 0 {
		dim = 4
		if t.VectorDim >= 2 {
			dim = t.VectorDim
		}
	}

	r.check("flush_empties_store", func(ctx context.Context) error {
		if t.RequireEmpty {
			n, err := vs.Size(ctx)
			if err != nil {
				return err
			}
			if n != 0 {
				return fmt.Errorf("store holds %d vectors; refusing to run destructive checks against a non-empty store", n)
			}
		}
		if _, err := vs.Upsert(ctx, unit(dim, 0)); err != nil {
			return err
		}
		if err := vs.Flush(ctx); err != nil {
			return err
		}
		n, err := vs.Size(ctx)
		if err != nil {
			return err
		}
		if n != 0 {
			return fmt.Errorf("size after flush = %d, want 0", n)
		}
		return nil
	})

	var idA, idB, idC, idD, idF string
	r.check("upsert_returns_unique_safe_ids", func(ctx context.Context) error {
		var err error
		ups := []struct {
			id *string
			v  []float64
		}{{&idA, unit(dim, 0)}, {&idB, mix(dim, 0.3)}, {&idC, mix(dim, 1)}, {&idD, unit(dim, 1)}, {&idF, scale(unit(dim, 0), -1)}}
		seen := map[string]bool{}
		for i, u := range ups {
			if *u.id, err = vs.Upsert(ctx, u.v); err != nil {
				return err
			}
			if seen[*u.id] {
				return fmt.Errorf("upsert #%d returned a duplicate id", i)
			}
			seen[*u.id] = true
		}
		n, err := vs.Size(ctx)
		if err != nil {
			return err
		}
		if n != len(ups) {
			return fmt.Errorf("size = %d after %d upserts", n, len(ups))
		}
		return nil
	})

	q := unit(dim, 0)
	r.check("nearest_is_identical_vector", func(ctx context.Context) error {
		ms, err := vs.Search(ctx, q, 1, huge)
		if err != nil {
			return err
		}
		if len(ms) != 1 || ms[0].ID != idA {
			return fmt.Errorf("nearest match = %+v, want the identical vector %s", ms, idA)
		}
		if ms[0].Distance > 1e-4 || ms[0].Distance < -1e-4 {
			return fmt.Errorf("distance of identical vector = %v, want ~0 (lower is more similar)", ms[0].Distance)
		}
		return nil
	})
	r.check("top_k_and_best_to_worst_order", func(ctx context.Context) error {
		all, err := vs.Search(ctx, q, 10, huge)
		if err != nil {
			return err
		}
		if len(all) != 5 {
			return fmt.Errorf("top_k=10 over 5 vectors returned %d matches", len(all))
		}
		if !sort.SliceIsSorted(all, func(i, j int) bool { return all[i].Distance < all[j].Distance }) {
			return errors.New("matches are not ordered best-to-worst")
		}
		want := []string{idA, idB, idC, idD, idF}
		for i, m := range all {
			if m.ID != want[i] {
				return fmt.Errorf("rank %d is %s, want %s (distances %v)", i+1, m.ID, want[i], all)
			}
		}
		three, err := vs.Search(ctx, q, 3, huge)
		if err != nil {
			return err
		}
		if len(three) != 3 || three[0].ID != idA || three[2].ID != idC {
			return fmt.Errorf("top_k=3 returned %+v", three)
		}
		return nil
	})
	r.check("threshold_is_inclusive", func(ctx context.Context) error {
		all, err := vs.Search(ctx, q, 10, huge)
		if err != nil || len(all) < 3 {
			return fmt.Errorf("setup search failed: %v", err)
		}
		d := all[1].Distance // idB
		at, err := vs.Search(ctx, q, 10, d)
		if err != nil {
			return err
		}
		if !containsID(at, idB) {
			return fmt.Errorf("a match with distance == threshold (%v) was not returned; a hit requires distance <= threshold", d)
		}
		below, err := vs.Search(ctx, q, 10, d-math.Max(1e-6, math.Abs(d)*1e-3))
		if err != nil {
			return err
		}
		if containsID(below, idB) {
			return errors.New("a match with distance above the threshold was returned")
		}
		none, err := vs.Search(ctx, q, 10, -1)
		if err != nil {
			return err
		}
		if len(none) != 0 {
			return errors.New("a negative threshold returned matches")
		}
		return nil
	})
	r.check("wrong_dimension_rejected", func(ctx context.Context) error {
		if err := expect4xx(ctx, t.Client, http.MethodPost, "/v1/upsert", map[string]any{"vector": make([]float64, dim+1)}); err != nil {
			return err
		}
		return expect4xx(ctx, t.Client, http.MethodPost, "/v1/search", map[string]any{"vector": make([]float64, dim+1), "top_k": 1, "threshold": 1})
	})
	r.check("invalid_top_k_rejected", func(ctx context.Context) error {
		return expect4xx(ctx, t.Client, http.MethodPost, "/v1/search", map[string]any{"vector": q, "top_k": 0, "threshold": 1})
	})
	r.check("delete_semantics", func(ctx context.Context) error {
		if err := vs.Delete(ctx, idD); err != nil {
			return err
		}
		n, _ := vs.Size(ctx)
		if n != 4 {
			return fmt.Errorf("size after delete = %d, want 4", n)
		}
		all, err := vs.Search(ctx, q, 10, huge)
		if err != nil {
			return err
		}
		if containsID(all, idD) {
			return errors.New("a deleted vector is still returned by search")
		}
		// A second delete may report not-found (404) but must not be a server error.
		if err := vs.Delete(ctx, idD); err != nil {
			if s := statusOf(err); s != http.StatusNotFound {
				return fmt.Errorf("repeated delete failed with %v, want success or 404", err)
			}
		}
		return nil
	})
	r.check("rebuild_replaces_contents", func(ctx context.Context) error {
		set1 := []vectorstore.RebuildEntry{{ID: "contract-a", Vector: unit(dim, 0)}, {ID: "contract-b", Vector: unit(dim, 1)}, {ID: "contract-c", Vector: mix(dim, 0.3)}}
		if n, err := vs.Rebuild(ctx, set1); err != nil || n != 3 {
			return fmt.Errorf("rebuild(3) = %d, %v", n, err)
		}
		if n, _ := vs.Size(ctx); n != 3 {
			return fmt.Errorf("size after rebuild = %d, want 3", n)
		}
		ms, err := vs.Search(ctx, q, 1, huge)
		if err != nil || len(ms) != 1 || ms[0].ID != "contract-a" {
			return fmt.Errorf("after rebuild, nearest = %+v (%v), want contract-a with the caller-supplied id", ms, err)
		}
		if n, err := vs.Rebuild(ctx, []vectorstore.RebuildEntry{{ID: "contract-z", Vector: unit(dim, 1)}}); err != nil || n != 1 {
			return fmt.Errorf("rebuild(1) = %d, %v", n, err)
		}
		all, err := vs.Search(ctx, q, 10, huge)
		if err != nil || len(all) != 1 || all[0].ID != "contract-z" {
			return fmt.Errorf("rebuild merged instead of replacing: %+v (%v)", all, err)
		}
		if n, err := vs.Rebuild(ctx, nil); err != nil || n != 0 {
			return fmt.Errorf("rebuild(empty) = %d, %v", n, err)
		}
		if n, _ := vs.Size(ctx); n != 0 {
			return fmt.Errorf("size after empty rebuild = %d, want 0", n)
		}
		return nil
	})
	r.check("rebuild_rejects_wrong_dimension", func(ctx context.Context) error {
		return expect4xx(ctx, t.Client, http.MethodPost, "/v1/rebuild",
			map[string]any{"entries": []map[string]any{{"id": "contract-bad", "vector": make([]float64, dim+1)}}})
	})
	r.check("concurrent_upserts_and_searches", func(ctx context.Context) error {
		if err := vs.Flush(ctx); err != nil {
			return err
		}
		const workers, each = 8, 10
		if err := parallel(workers, func(w int) error {
			for i := 0; i < each; i++ {
				if _, err := vs.Upsert(ctx, unit(dim, w+i)); err != nil {
					return err
				}
				if _, err := vs.Search(ctx, unit(dim, w), 3, huge); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return err
		}
		n, err := vs.Size(ctx)
		if err != nil {
			return err
		}
		if n != workers*each {
			return fmt.Errorf("size = %d after %d concurrent upserts", n, workers*each)
		}
		return nil
	})
	r.check("usable_after_flush", func(ctx context.Context) error {
		if err := vs.Flush(ctx); err != nil {
			return err
		}
		if _, err := vs.Upsert(ctx, unit(dim, 0)); err != nil {
			return fmt.Errorf("store unusable after flush: %w", err)
		}
		return vs.Flush(ctx)
	})
	_ = time.Second
}

func scale(v []float64, k float64) []float64 {
	out := make([]float64, len(v))
	for i := range v {
		out[i] = v[i] * k
	}
	return out
}

func containsID(ms []vectorstore.Match, id string) bool {
	for _, m := range ms {
		if m.ID == id {
			return true
		}
	}
	return false
}
