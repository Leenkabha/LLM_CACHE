package protocol

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"

	"github.com/leenkabha/llm_cache/internal/vectorstore"
)

// Vector-store contract (v1):
//
//	POST   /v1/search       {"vector":[..],"top_k":3,"threshold":0.25} -> {"matches":[{"id":"a","distance":0.1}]}
//	POST   /v1/upsert       {"vector":[..]}                            -> {"id":"generated-id"}
//	DELETE /v1/entries/{id}                                             -> 200 (404 if unknown)
//	GET    /v1/size                                                     -> {"size":N}
//	POST   /v1/flush                                                    -> 200
//	POST   /v1/rebuild      {"entries":[{"id":"a","vector":[..]}]}      -> {"restored":N}
//	GET    /v1/info         optional {"dim":384,"metric":"cosine"} (also accepted as extra fields of /health)
//	GET    /health
//
// Semantics: lower distance is more similar; a hit needs distance <= threshold;
// matches are best-to-worst and at most top_k; rebuild REPLACES all contents;
// ids are stable, non-blank and safe as a URL path segment.

// VectorStore is the remote adapter for vectorstore.VectorStore.
type VectorStore struct{ c *Client }

var (
	_ vectorstore.VectorStore  = (*VectorStore)(nil)
	_ vectorstore.InfoProvider = (*VectorStore)(nil)
)

// MaxRebuildBytes bounds the rebuild request/response handling; the request
// itself is limited by the adapter refusing more than MaxRebuildEntries.
const MaxRebuildEntries = 2_000_000

func NewVectorStore(c *Client) *VectorStore {
	cc := *c
	if cc.MaxResponse == 0 || cc.MaxResponse > 4<<20 {
		cc.MaxResponse = 4 << 20
	}
	if cc.Timeout == DefaultTimeout {
		cc.Timeout = 30e9 // rebuilds of large caches take longer than a search
	}
	return &VectorStore{c: &cc}
}

type vsSearchRequest struct {
	Vector    []float64 `json:"vector"`
	TopK      int       `json:"top_k"`
	Threshold float64   `json:"threshold"`
}

type vsSearchResponse struct {
	Matches []vectorstore.Match `json:"matches"`
}

func (v *VectorStore) Search(ctx context.Context, vec []float64, topK int, threshold float64) ([]vectorstore.Match, error) {
	if topK < 1 {
		return nil, errors.New("topK must be at least 1")
	}
	var out vsSearchResponse
	if _, err := v.c.Do(ctx, http.MethodPost, "/v1/search", vsSearchRequest{Vector: vec, TopK: topK, Threshold: threshold}, &out); err != nil {
		return nil, fmt.Errorf("vector-store plugin: %w", err)
	}
	if out.Matches == nil {
		return nil, fmt.Errorf("vector-store plugin: %w (missing matches array)", ErrMalformed)
	}
	if len(out.Matches) > topK {
		return nil, fmt.Errorf("vector-store plugin returned %d matches for top_k=%d", len(out.Matches), topK)
	}
	prev := math.Inf(-1)
	for _, m := range out.Matches {
		switch {
		case !ValidID(m.ID):
			return nil, errors.New("vector-store plugin returned an unsafe or blank id")
		case math.IsNaN(m.Distance) || math.IsInf(m.Distance, 0):
			return nil, errors.New("vector-store plugin returned a non-finite distance")
		case m.Distance > threshold:
			return nil, errors.New("vector-store plugin returned a match above the threshold")
		case m.Distance < prev:
			return nil, errors.New("vector-store plugin returned matches out of best-to-worst order")
		}
		prev = m.Distance
	}
	return out.Matches, nil
}

type vsUpsertRequest struct {
	Vector []float64 `json:"vector"`
}

type vsUpsertResponse struct {
	ID string `json:"id"`
}

func (v *VectorStore) Upsert(ctx context.Context, vec []float64) (string, error) {
	var out vsUpsertResponse
	if _, err := v.c.Do(ctx, http.MethodPost, "/v1/upsert", vsUpsertRequest{Vector: vec}, &out); err != nil {
		return "", fmt.Errorf("vector-store plugin: %w", err)
	}
	if !ValidID(out.ID) {
		return "", errors.New("vector-store plugin returned an unsafe or blank id")
	}
	return out.ID, nil
}

func (v *VectorStore) Delete(ctx context.Context, id string) error {
	if !ValidID(id) {
		return errors.New("invalid entry id")
	}
	if _, err := v.c.Do(ctx, http.MethodDelete, "/v1/entries/"+url.PathEscape(id), nil, nil); err != nil {
		return fmt.Errorf("vector-store plugin: %w", err)
	}
	return nil
}

func (v *VectorStore) Size(ctx context.Context) (int, error) {
	var out struct {
		Size *int `json:"size"`
	}
	if _, err := v.c.Do(ctx, http.MethodGet, "/v1/size", nil, &out); err != nil {
		return 0, fmt.Errorf("vector-store plugin: %w", err)
	}
	if out.Size == nil || *out.Size < 0 {
		return 0, fmt.Errorf("vector-store plugin: %w", ErrMalformed)
	}
	return *out.Size, nil
}

func (v *VectorStore) Flush(ctx context.Context) error {
	if _, err := v.c.Do(ctx, http.MethodPost, "/v1/flush", struct{}{}, nil); err != nil {
		return fmt.Errorf("vector-store plugin: %w", err)
	}
	return nil
}

func (v *VectorStore) Rebuild(ctx context.Context, entries []vectorstore.RebuildEntry) (int, error) {
	if len(entries) > MaxRebuildEntries {
		return 0, fmt.Errorf("refusing to rebuild %d entries (limit %d)", len(entries), MaxRebuildEntries)
	}
	for _, e := range entries {
		if !ValidID(e.ID) {
			return 0, fmt.Errorf("cannot rebuild: entry id %q is not safe as a URL segment", e.ID)
		}
	}
	if entries == nil {
		entries = []vectorstore.RebuildEntry{}
	}
	var out struct {
		Restored *int `json:"restored"`
	}
	req := struct {
		Entries []vectorstore.RebuildEntry `json:"entries"`
	}{entries}
	if _, err := v.c.Do(ctx, http.MethodPost, "/v1/rebuild", req, &out); err != nil {
		return 0, fmt.Errorf("vector-store plugin: %w", err)
	}
	if out.Restored == nil || *out.Restored != len(entries) {
		return 0, fmt.Errorf("vector-store plugin restored %v of %d entries", derefInt(out.Restored), len(entries))
	}
	return *out.Restored, nil
}

func derefInt(p *int) any {
	if p == nil {
		return "unknown"
	}
	return *p
}

// Info implements vectorstore.InfoProvider. It reads the optional GET /v1/info
// ({"dim":384,"metric":"cosine"}) and falls back to the same optional fields on
// GET /health.
func (v *VectorStore) Info(ctx context.Context) (vectorstore.Info, error) {
	var in struct {
		Dim    int    `json:"dim"`
		Metric string `json:"metric"`
	}
	if status, err := v.c.Do(ctx, http.MethodGet, "/v1/info", nil, &in); err == nil && status == http.StatusOK && in.Dim > 0 {
		return vectorstore.Info{Dim: in.Dim, Metric: in.Metric}, nil
	}
	h, err := v.c.HealthDetail(ctx)
	if err != nil {
		return vectorstore.Info{}, fmt.Errorf("vector-store plugin: %w", err)
	}
	return vectorstore.Info{Dim: h.Dim, Metric: h.Metric}, nil
}

func (v *VectorStore) Health(ctx context.Context) error { return v.c.Health(ctx) }
