package protocol

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"

	"github.com/leenkabha/llm_cache/internal/embedder"
)

// Embedder contract (v1):
//
//	POST /v1/embed       {"text":"hello"} -> 200 {"vector":[...],"dim":384,"model":"name"}
//	GET  /v1/model-info                   -> 200 {"name":"model","dim":384}
//	GET  /health
//
// Vectors must be finite and len(vector) must equal dim. Vectors should be unit
// length when the cache uses cosine distance.

// MaxEmbeddingDim bounds a vector so a hostile plugin cannot exhaust memory.
const MaxEmbeddingDim = 8192

// Embedder is the remote adapter for embedder.Embedder.
type Embedder struct{ c *Client }

var (
	_ embedder.Embedder          = (*Embedder)(nil)
	_ embedder.ModelInfoProvider = (*Embedder)(nil)
)

func NewEmbedder(c *Client) *Embedder {
	cc := *c
	if cc.MaxResponse == 0 || cc.MaxResponse > 4<<20 {
		cc.MaxResponse = 4 << 20
	}
	return &Embedder{c: &cc}
}

type embedRequest struct {
	Text string `json:"text"`
}

type embedResponse struct {
	Vector []float64 `json:"vector"`
	Dim    int       `json:"dim"`
	Model  string    `json:"model"`
}

// Embed implements embedder.Embedder.
func (e *Embedder) Embed(ctx context.Context, text string) ([]float64, error) {
	v, _, err := e.EmbedDetailed(ctx, text)
	return v, err
}

// EmbedDetailed also returns the model name the plugin reported.
func (e *Embedder) EmbedDetailed(ctx context.Context, text string) ([]float64, string, error) {
	var out embedResponse
	if _, err := e.c.Do(ctx, http.MethodPost, "/v1/embed", embedRequest{Text: text}, &out); err != nil {
		return nil, "", fmt.Errorf("embedder plugin: %w", err)
	}
	if err := ValidateVector(out.Vector, out.Dim); err != nil {
		return nil, "", fmt.Errorf("embedder plugin: %w", err)
	}
	return out.Vector, out.Model, nil
}

// ValidateVector checks the invariants every embedding must satisfy.
func ValidateVector(v []float64, dim int) error {
	if len(v) == 0 {
		return errors.New("empty vector")
	}
	if len(v) > MaxEmbeddingDim {
		return fmt.Errorf("vector has %d components, above the limit of %d", len(v), MaxEmbeddingDim)
	}
	if dim != len(v) {
		return fmt.Errorf("reported dim %d does not match vector length %d", dim, len(v))
	}
	for _, x := range v {
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return errors.New("vector contains a non-finite value")
		}
	}
	return nil
}

// ModelInfo implements embedder.ModelInfoProvider.
func (e *Embedder) ModelInfo(ctx context.Context) (embedder.ModelInfo, error) {
	var out embedder.ModelInfo
	if _, err := e.c.Do(ctx, http.MethodGet, "/v1/model-info", nil, &out); err != nil {
		return embedder.ModelInfo{}, fmt.Errorf("embedder plugin: %w", err)
	}
	if out.Name == "" || out.Dim < 1 || out.Dim > MaxEmbeddingDim {
		return embedder.ModelInfo{}, fmt.Errorf("embedder plugin: %w", ErrMalformed)
	}
	return out, nil
}

// Health checks GET /health.
func (e *Embedder) Health(ctx context.Context) error { return e.c.Health(ctx) }
