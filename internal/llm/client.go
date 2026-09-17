// Package llm encapsulates the remote LLM backend behind a single interface,
// so the demo provider (OpenAI) can be swapped for any other.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/leenkabha/llm_cache/internal/config"
	"github.com/leenkabha/llm_cache/internal/plugin"
)

const openAIResponsesURL = "https://api.openai.com/v1/responses"
const geminiAPIBase = "https://generativelanguage.googleapis.com/v1beta/models"

// Backend is the contract the orchestrator depends on for completions.
//
// See docs/EXTENDING.md and example_http.go for a complete extension example.
// HTTP implementations must honor ctx and be safe for concurrent Complete calls.
// This is the pluggable seam: the orchestrator holds a Backend, so the concrete
// LLM provider behind it is interchangeable.
type Backend interface {
	Complete(ctx context.Context, prompt string) (string, error)
}

// Mode names the built-in LLM backends selectable via configuration.
const (
	// ModeStub returns canned replies with no network access (default).
	ModeStub = "stub"
	// ModeOpenAI calls the OpenAI Responses API.
	ModeOpenAI = "openai"
	// ModeGemini calls the Google Generative Language (Gemini) API.
	ModeGemini = "gemini"
)

// registry holds every LLM backend, keyed by the name used in LLM_MODE.
var registry = plugin.NewRegistry[Backend]("llm mode")

// Register makes an LLM backend available under name.
//
// Call it from an init() in your adapter file. Because adapters live in package
// llm, adding the file is enough to register the backend -- no existing file,
// and in particular not the orchestrator, needs to change. Then set
// LLM_MODE=<name> to select it.
func Register(name string, factory plugin.Factory[Backend]) {
	registry.Register(name, factory)
}

// New builds the Backend selected by cfg.LLMMode from the registry. If
// cfg.LLMFallbackMode is set, the result is wrapped so that a failure of the
// primary backend (for any reason other than context cancellation) falls
// back to the named backend instead of failing the request -- see
// fallback.go. Leaving LLMFallbackMode empty (the default) reproduces the
// previous behavior exactly: no wrapping, a primary-backend failure fails
// the request.
func New(cfg config.Config) (Backend, error) {
	name := cfg.LLMMode
	if name == "" {
		name = ModeStub
	}
	primary, err := registry.Build(name, cfg)
	if err != nil {
		return nil, err
	}
	if cfg.LLMFallbackMode == "" {
		return primary, nil
	}
	fallback, err := registry.Build(cfg.LLMFallbackMode, cfg)
	if err != nil {
		return nil, fmt.Errorf("build llm fallback: %w", err)
	}
	return NewFallback(primary, fallback), nil
}

// The built-in backends register themselves. New adapters follow the same shape
// in their own files -- this init() is not an extension point, just where the
// defaults live.
func init() {
	Register(ModeStub, func(config.Config) (Backend, error) {
		return &stubBackend{}, nil
	})
	Register(ModeOpenAI, func(cfg config.Config) (Backend, error) {
		return &openAIBackend{
			apiKey: cfg.OpenAIKey,
			model:  cfg.OpenAIModel,
			http:   &http.Client{Timeout: 30 * time.Second},
		}, nil
	})
	Register(ModeGemini, func(cfg config.Config) (Backend, error) {
		return &geminiBackend{
			apiKey:  cfg.GeminiKey,
			model:   cfg.GeminiModel,
			baseURL: geminiAPIBase,
			http:    &http.Client{Timeout: 30 * time.Second},
		}, nil
	})
}

// stubBackend returns a canned reply so the skeleton runs with no network
// access or API key. Swap LLM_MODE=openai once a key is available.
type stubBackend struct{}

func (s *stubBackend) Complete(_ context.Context, prompt string) (string, error) {
	return fmt.Sprintf("[stub-llm] This is a placeholder reply for: %q", prompt), nil
}

type openAIBackend struct {
	apiKey string
	model  string
	http   *http.Client
}

type responsesRequest struct {
	Model string `json:"model"`
	Input string `json:"input"`
}

type responsesResponse struct {
	OutputText string `json:"output_text"`
	Output     []struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	} `json:"output"`
}

func (o *openAIBackend) Complete(ctx context.Context, prompt string) (string, error) {
	if o.apiKey == "" {
		return "", fmt.Errorf("OPENAI_API_KEY not set")
	}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		reply, err := o.completeOnce(ctx, prompt)
		if err == nil {
			return reply, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		time.Sleep(time.Duration(attempt+1) * 500 * time.Millisecond)
	}
	return "", lastErr
}

func (o *openAIBackend) completeOnce(ctx context.Context, prompt string) (string, error) {
	body, err := json.Marshal(responsesRequest{Model: o.model, Input: prompt})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, openAIResponsesURL, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+o.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := o.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("openai unreachable: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("openai returned %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}

	var out responsesResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return "", err
	}
	reply := extractResponseText(out)
	if reply == "" {
		return "", fmt.Errorf("openai response did not contain text")
	}
	return reply, nil
}

func extractResponseText(resp responsesResponse) string {
	if strings.TrimSpace(resp.OutputText) != "" {
		return strings.TrimSpace(resp.OutputText)
	}
	var parts []string
	for _, output := range resp.Output {
		for _, content := range output.Content {
			if text := strings.TrimSpace(content.Text); text != "" {
				parts = append(parts, text)
			}
		}
	}
	return strings.Join(parts, "\n")
}

// geminiBackend calls the Google Generative Language API (Gemini), which
// offers a free tier suitable for demos without OpenAI billing.
type geminiBackend struct {
	apiKey  string
	model   string
	baseURL string // defaults to geminiAPIBase; overridable in tests
	http    *http.Client
}

type geminiRequest struct {
	Contents []geminiContent `json:"contents"`
}

type geminiContent struct {
	Parts []geminiPart `json:"parts"`
}

type geminiPart struct {
	Text string `json:"text"`
}

type geminiResponse struct {
	Candidates []struct {
		Content struct {
			Parts []geminiPart `json:"parts"`
		} `json:"content"`
	} `json:"candidates"`
}

// geminiMaxAttempts and geminiInitialBackoff bound Gemini's retry loop.
// Backoff doubles each attempt: 500ms, 1s, 2s, 4s -- about 7.5s of sleep
// across 5 attempts, which is meaningfully more forgiving of a short
// capacity spike than a fixed 3-attempt/1.5s-total retry without drawing
// out a request indefinitely.
const (
	geminiMaxAttempts    = 5
	geminiInitialBackoff = 500 * time.Millisecond
)

func (g *geminiBackend) Complete(ctx context.Context, prompt string) (string, error) {
	if g.apiKey == "" {
		return "", fmt.Errorf("GEMINI_API_KEY not set")
	}
	var lastErr error
	backoff := geminiInitialBackoff
	for attempt := 0; attempt < geminiMaxAttempts; attempt++ {
		reply, retryable, err := g.completeOnce(ctx, prompt)
		if err == nil {
			return reply, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if !retryable {
			return "", err
		}
		if attempt < geminiMaxAttempts-1 {
			time.Sleep(backoff)
			backoff *= 2
		}
	}
	return "", lastErr
}

// isDailyQuotaExhausted reports whether a 429 response body is Google's
// structured per-day quota error (quotaId containing "PerDay..."), as
// opposed to a short-term rate limit. A per-day quota cannot recover within
// any retry backoff this process would reasonably wait, so retrying it only
// burns the fallback's time budget on a request that is certain to fail
// again; a short-term rate limit genuinely might clear within a few seconds,
// so it stays retryable. This is a plain substring check rather than full
// JSON parsing of Google's error-details schema because the fields involved
// are typed as free-form protobuf Any values -- matching the one field name
// that actually distinguishes the two cases is simpler and just as reliable.
func isDailyQuotaExhausted(body []byte) bool {
	return bytes.Contains(body, []byte("PerDay"))
}

// completeOnce reports whether a failure is worth retrying: transient
// network errors and HTTP 429/5xx responses are (the server may recover);
// everything else -- a malformed request, an auth failure, a response with
// no usable text, or a 429 that is specifically a per-day quota exhaustion --
// will fail identically on every retry, so retrying it would only waste the
// fallback's time budget for no benefit.
func (g *geminiBackend) completeOnce(ctx context.Context, prompt string) (reply string, retryable bool, err error) {
	body, err := json.Marshal(geminiRequest{Contents: []geminiContent{{Parts: []geminiPart{{Text: prompt}}}}})
	if err != nil {
		return "", false, err
	}
	url := fmt.Sprintf("%s/%s:generateContent?key=%s", g.baseURL, g.model, g.apiKey)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", false, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := g.http.Do(req)
	if err != nil {
		return "", true, fmt.Errorf("gemini unreachable: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", true, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		retryableStatus := (resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500) && !isDailyQuotaExhausted(data)
		return "", retryableStatus, fmt.Errorf("gemini returned %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}

	var out geminiResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return "", false, err
	}
	if len(out.Candidates) == 0 {
		return "", false, fmt.Errorf("gemini response did not contain text")
	}
	var parts []string
	for _, p := range out.Candidates[0].Content.Parts {
		if text := strings.TrimSpace(p.Text); text != "" {
			parts = append(parts, text)
		}
	}
	reply = strings.Join(parts, "\n")
	if reply == "" {
		return "", false, fmt.Errorf("gemini response did not contain text")
	}
	return reply, false, nil
}
