// Package embedder is the orchestrator's client for the embedding service.
// It depends only on the REST contract, so the underlying model
// (sentence-transformers today, anything else later) is interchangeable.
package embedder

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

type Client struct {
	baseURL string
	http    *http.Client
}

func New(baseURL string) *Client {
	return &Client{
		baseURL: baseURL,
		http:    &http.Client{Timeout: 10 * time.Second},
	}
}

type embedRequest struct {
	Text string `json:"text"`
}

type embedResponse struct {
	Vector []float64 `json:"vector"`
	Dim    int       `json:"dim"`
}

// Embed converts a prompt into an embedding vector via POST /embed.
func (c *Client) Embed(ctx context.Context, text string) ([]float64, error) {
	body, _ := json.Marshal(embedRequest{Text: text})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/embed", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embedding service unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("embedding service returned %d", resp.StatusCode)
	}

	var out embedResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out.Vector, nil
}

func (c *Client) Health(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/health", nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("embedding service unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("embedding service returned %d", resp.StatusCode)
	}
	return nil
}
