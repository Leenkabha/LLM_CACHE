package contract

import (
	"context"
	"fmt"
	"net/http"
	"strings"
)

// The three Python plugin types run inside a specialised runner image that
// exposes the same protocols as the Go-side plugins plus a small introspection
// API used to prove the developer's package was discovered and selected:
//
//	GET  /v1/runner-info     {"kind":"embedding|vector","backend":"name","metric":"name","registered_backends":[..],"registered_metrics":[..]}
//	GET  /v1/metric-info     {"name":"euclid2","faiss_metric_type":1}
//	POST /v1/metric/distance {"scores":[..]} -> {"distances":[..]}

type runnerInfo struct {
	Kind               string   `json:"kind"`
	Backend            string   `json:"backend"`
	Metric             string   `json:"metric"`
	RegisteredBackends []string `json:"registered_backends"`
	RegisteredMetrics  []string `json:"registered_metrics"`
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func checkRunner(r *runner, t Target, kind string, metric bool) {
	r.check("plugin_discovery", func(ctx context.Context) error {
		var info runnerInfo
		if _, err := t.Client.Do(ctx, http.MethodGet, "/v1/runner-info", nil, &info); err != nil {
			return fmt.Errorf("runner-info: %w", err)
		}
		if info.Kind != kind {
			return fmt.Errorf("runner kind is %q, want %q", info.Kind, kind)
		}
		want := t.ExpectedBackend
		if want == "" {
			return errSkip("no expected backend name supplied")
		}
		got, registered := info.Backend, info.RegisteredBackends
		if metric {
			got, registered = info.Metric, info.RegisteredMetrics
		}
		if !contains(registered, want) {
			return fmt.Errorf("the plugin did not register %q (registered: %s); is the @register(%q) decorator executed on import?", want, strings.Join(registered, ", "), want)
		}
		if got != want {
			return fmt.Errorf("runner selected %q instead of %q", got, want)
		}
		return nil
	})
}

func runEmbeddingModel(r *runner, t Target) {
	checkRunner(r, t, "embedding", false)
	runEmbedder(r, t)
}

func runVectorIndex(r *runner, t Target) {
	checkRunner(r, t, "vector", false)
	runVectorStore(r, t)
}

func runSimilarityMetric(r *runner, t Target) {
	checkRunner(r, t, "vector", true)
	var faissType int = -1
	r.check("metric_identity", func(ctx context.Context) error {
		var info struct {
			Name string `json:"name"`
			Type *int   `json:"faiss_metric_type"`
		}
		if _, err := t.Client.Do(ctx, http.MethodGet, "/v1/metric-info", nil, &info); err != nil {
			return fmt.Errorf("metric-info: %w", err)
		}
		if info.Name == "" || info.Type == nil {
			return fmt.Errorf("metric-info must report name and faiss_metric_type")
		}
		if t.ExpectedBackend != "" && info.Name != t.ExpectedBackend {
			return fmt.Errorf("metric reports name %q, manifest says %q", info.Name, t.ExpectedBackend)
		}
		faissType = *info.Type
		return nil
	})
	r.check("compatible_with_faiss_index", func(ctx context.Context) error {
		// FAISS METRIC_INNER_PRODUCT = 0 and METRIC_L2 = 1 are what the built-in
		// index can compute; anything else cannot run on it.
		if faissType != 0 && faissType != 1 {
			return fmt.Errorf("faiss_metric_type %d is not supported by the built-in FAISS index (0=inner product, 1=L2)", faissType)
		}
		return nil
	})
	r.check("score_conversion_lower_is_better", func(ctx context.Context) error {
		scores := []float64{0, 0.25, 1, 4}
		if faissType == 0 {
			scores = []float64{-1, -0.5, 0, 0.5, 0.9, 1}
		}
		var out struct {
			Distances []float64 `json:"distances"`
		}
		if _, err := t.Client.Do(ctx, http.MethodPost, "/v1/metric/distance", map[string]any{"scores": scores}, &out); err != nil {
			return fmt.Errorf("metric/distance: %w", err)
		}
		if len(out.Distances) != len(scores) {
			return fmt.Errorf("got %d distances for %d scores", len(out.Distances), len(scores))
		}
		for i, d := range out.Distances {
			if d != d || d > 1e12 || d < -1e12 {
				return fmt.Errorf("distance %d is not finite", i)
			}
			if i == 0 {
				continue
			}
			prev := out.Distances[i-1]
			if faissType == 0 && d > prev+1e-9 {
				return fmt.Errorf("inner-product scores rise (%v -> %v) but distance rose (%v -> %v); a better score must give a lower-or-equal distance", scores[i-1], scores[i], prev, d)
			}
			if faissType == 1 && d < prev-1e-9 {
				return fmt.Errorf("L2 scores rise (%v -> %v) but distance fell (%v -> %v); a worse score must give a higher-or-equal distance", scores[i-1], scores[i], prev, d)
			}
		}
		return nil
	})
	// Ordering and threshold behaviour end to end, through the real index.
	runVectorStore(r, t)
}
