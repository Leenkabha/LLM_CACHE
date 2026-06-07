// Package llm encapsulates the remote LLM backend behind a single interface,
// so the demo provider (OpenAI) can be swapped for any other.
package llm

import (
	"context"
	"fmt"
)

// Backend is the contract the orchestrator depends on for completions.
type Backend interface {
	Complete(ctx context.Context, prompt string) (string, error)
}

// New returns a Backend based on mode: "stub" (default) or "openai".
func New(mode, apiKey, model string) Backend {
	switch mode {
	case "openai":
		return &openAIBackend{apiKey: apiKey, model: model}
	default:
		return &stubBackend{}
	}
}

// stubBackend returns a canned reply so the skeleton runs with no network
// access or API key. Swap LLM_MODE=openai once a key is available.
type stubBackend struct{}

func (s *stubBackend) Complete(_ context.Context, prompt string) (string, error) {
	return fmt.Sprintf("[stub-llm] This is a placeholder reply for: %q", prompt), nil
}

// openAIBackend is a placeholder for the real OpenAI integration.
// TODO(week 8): call the OpenAI completions API with timeout + retry.
type openAIBackend struct {
	apiKey string
	model  string
}

func (o *openAIBackend) Complete(_ context.Context, prompt string) (string, error) {
	if o.apiKey == "" {
		return "", fmt.Errorf("OPENAI_API_KEY not set")
	}
	return "", fmt.Errorf("openai backend not implemented yet")
}
