// Package vectorstore is the orchestrator's client for the vector-store service.
// The demo backend is an in-memory brute-force search; the same REST contract
// will front a FAISS index later without changing this client.
package vectorstore

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

// Match is the nearest stored vector for a query.
type Match struct {
	ID       string  `json:"id"`
	Distance float64 `json:"distance"`
}

type searchRequest struct {
	Vector    []float64 `json:"vector"`
	TopK      int       `json:"top_k"`
	Threshold float64   `json:"threshold"`
}

type searchResponse struct {
	Hit  bool    `json:"hit"`
	ID   string  `json:"id"`
	Dist float64 `json:"distance"`
}

// Search returns the nearest entry within threshold, or nil on a miss.
func (c *Client) Search(ctx context.Context, vec []float64, topK int, threshold float64) (*Match, error) {
	body, _ := json.Marshal(searchRequest{Vector: vec, TopK: topK, Threshold: threshold})
	var out searchResponse
	if err := c.post(ctx, "/search", body, &out); err != nil {
		return nil, err
	}
	if !out.Hit {
		return nil, nil
	}
	return &Match{ID: out.ID, Distance: out.Dist}, nil
}

type upsertRequest struct {
	Vector []float64 `json:"vector"`
}

type upsertResponse struct {
	ID string `json:"id"`
}

// Upsert stores a vector and returns its generated id.
func (c *Client) Upsert(ctx context.Context, vec []float64) (string, error) {
	body, _ := json.Marshal(upsertRequest{Vector: vec})
	var out upsertResponse
	if err := c.post(ctx, "/upsert", body, &out); err != nil {
		return "", err
	}
	return out.ID, nil
}

// Delete removes a vector by id (used by eviction).
func (c *Client) Delete(ctx context.Context, id string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.baseURL+"/entries/"+id, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

type sizeResponse struct {
	Size int `json:"size"`
}

// Size returns the number of stored vectors.
func (c *Client) Size(ctx context.Context) (int, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/size", nil)
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	var out sizeResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, err
	}
	return out.Size, nil
}

// Flush clears all stored vectors.
func (c *Client) Flush(ctx context.Context) error {
	return c.post(ctx, "/flush", []byte("{}"), &struct{}{})
}

func (c *Client) post(ctx context.Context, path string, body []byte, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("vector store unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("vector store returned %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
