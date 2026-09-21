package protocol

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"

	"github.com/leenkabha/llm_cache/internal/llm"
)

// LLM contract (v1):
//
//	POST /v1/complete  {"model":"m","prompt":"p"}  ->  200 {"reply":"text"}
//	GET  /v1/usage     (optional)                  ->  200 llm.UsageSnapshot JSON, 404 if unsupported
//	GET  /health

// LLM is the remote adapter for llm.Backend.
type LLM struct {
	c     *Client
	model string
}

var (
	_ llm.Backend       = (*LLM)(nil)
	_ llm.UsageReporter = (*LLM)(nil)
)

// NewLLM builds the adapter. c.MaxResponse is capped at MaxReplyBytes plus JSON
// overhead.
func NewLLM(c *Client, model string) *LLM {
	cc := *c
	if cc.MaxResponse == 0 || cc.MaxResponse > MaxReplyBytes+4096 {
		cc.MaxResponse = MaxReplyBytes + 4096
	}
	return &LLM{c: &cc, model: model}
}

type completeRequest struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
}

type completeResponse struct {
	Reply string `json:"reply"`
}

func (l *LLM) Complete(ctx context.Context, prompt string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	var out completeResponse
	if _, err := l.c.Do(ctx, http.MethodPost, "/v1/complete", completeRequest{Model: l.model, Prompt: prompt}, &out); err != nil {
		if errors.Is(err, ErrTooLarge) {
			return "", fmt.Errorf("llm plugin response exceeds 1 MiB")
		}
		return "", fmt.Errorf("llm plugin: %w", err)
	}
	if strings.TrimSpace(out.Reply) == "" {
		return "", errors.New("llm plugin returned an empty reply")
	}
	if len(out.Reply) > MaxReplyBytes {
		return "", errors.New("llm plugin response exceeds 1 MiB")
	}
	return out.Reply, nil
}

// Usage implements llm.UsageReporter. A plugin that does not implement the
// optional endpoint simply reports no usage.
func (l *LLM) Usage() (llm.UsageSnapshot, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 2e9)
	defer cancel()
	var u llm.UsageSnapshot
	status, err := l.c.Do(ctx, http.MethodGet, "/v1/usage", nil, &u)
	if err != nil || status != http.StatusOK {
		return llm.UsageSnapshot{}, false
	}
	if u.Requests < 0 || u.Tokens < 0 || u.ResetsInSeconds < 0 || math.IsNaN(float64(u.Requests)) {
		return llm.UsageSnapshot{}, false
	}
	return u, true
}

// Health checks GET /health.
func (l *LLM) Health(ctx context.Context) error { return l.c.Health(ctx) }
