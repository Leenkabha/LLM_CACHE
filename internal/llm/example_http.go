package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/leenkabha/llm_cache/internal/config"
)

// ModeExampleHTTP is a complete custom-provider template, not a vendor API.
// Contract: POST {"model":"...","prompt":"..."} -> {"reply":"..."}.
const ModeExampleHTTP = "example-http"
const exampleMaxReplyBytes = 1 << 20

func init() {
	Register(ModeExampleHTTP, func(config.Config) (Backend, error) {
		return newExampleHTTP(os.Getenv("EXAMPLE_LLM_URL"), os.Getenv("EXAMPLE_LLM_MODEL"),
			os.Getenv("EXAMPLE_LLM_TOKEN"), os.Getenv("EXAMPLE_LLM_TIMEOUT"))
	})
}

type exampleHTTPBackend struct {
	endpoint, model, token string
	client                 *http.Client
}

var _ Backend = (*exampleHTTPBackend)(nil)

func newExampleHTTP(endpoint, model, token, timeoutText string) (*exampleHTTPBackend, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed == nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" || parsed.User != nil || parsed.Fragment != "" {
		return nil, fmt.Errorf("EXAMPLE_LLM_URL must be an absolute HTTP(S) URL without user information or fragment")
	}
	if strings.TrimSpace(model) == "" {
		return nil, fmt.Errorf("EXAMPLE_LLM_MODEL is required")
	}
	timeout := 30 * time.Second
	if timeoutText != "" {
		timeout, err = time.ParseDuration(timeoutText)
		if err != nil || timeout <= 0 {
			return nil, fmt.Errorf("EXAMPLE_LLM_TIMEOUT must be a positive duration, e.g. 30s")
		}
	}
	return &exampleHTTPBackend{endpoint: endpoint, model: model, token: token, client: &http.Client{
		Timeout: timeout,
		// Do not forward credentials to redirects; configure the final endpoint.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

func (b *exampleHTTPBackend) Complete(ctx context.Context, prompt string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	body, err := json.Marshal(struct {
		Model  string `json:"model"`
		Prompt string `json:"prompt"`
	}{b.model, prompt})
	if err != nil {
		return "", fmt.Errorf("encode example LLM request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.endpoint, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("create example LLM request")
	}
	req.Header.Set("Content-Type", "application/json")
	if b.token != "" {
		req.Header.Set("Authorization", "Bearer "+b.token)
	}
	resp, err := b.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		// Transport errors may contain endpoint query credentials; do not expose them.
		return "", fmt.Errorf("example LLM request failed or timed out")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("example LLM returned HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, exampleMaxReplyBytes+1))
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", fmt.Errorf("read example LLM response failed")
	}
	if len(data) > exampleMaxReplyBytes {
		return "", fmt.Errorf("example LLM response exceeds 1 MiB")
	}
	var out struct {
		Reply string `json:"reply"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", fmt.Errorf("example LLM returned invalid JSON")
	}
	if strings.TrimSpace(out.Reply) == "" {
		return "", fmt.Errorf("example LLM returned an empty reply")
	}
	return out.Reply, nil
}
