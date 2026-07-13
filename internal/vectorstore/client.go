// Package vectorstore is the orchestrator's client for the vector-store service.
// The backend is a FAISS index (see vector_store_service/app/main.py); this
// client just speaks the REST contract, so it never needed to change when
// the backend swapped from brute-force to FAISS.
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
//
// FIX: this previously ignored the response status code entirely, so a
// 404 (or any other error) from the vector store was silently treated as
// a success. That's dangerous specifically because eviction relies on
// Delete() actually working -- if it silently "succeeds" without really
// removing the vector, the vector store and Redis fall out of sync (the
// same failure mode IndexIDMap was introduced to prevent on the FAISS
// side -- see vector_store_service/app/main.py). Now it checks the
// status code the same way every other method here already does via
// post(), and reports a proper error if the deletion didn't actually
// happen.
func (c *Client) Delete(ctx context.Context, id string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.baseURL+"/entries/"+id, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("vector store unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("vector store returned %d", resp.StatusCode)
	}
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

func (c *Client) Health(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/health", nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("vector store unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("vector store returned %d", resp.StatusCode)
	}
	return nil
}

// RebuildEntry pairs a previously-saved reply's id with its vector, for
// restoring the FAISS index after a restart. Mirrors RebuildEntry in
// vector_store_service/app/main.py exactly -- field names and JSON tags
// must match that contract.
type RebuildEntry struct {
	ID     string    `json:"id"`
	Vector []float64 `json:"vector"`
}

type rebuildRequest struct {
	Entries []RebuildEntry `json:"entries"`
}

type rebuildResponse struct {
	Restored int `json:"restored"`
}

// Rebuild repopulates the RAM-only FAISS index from durably-stored entries.
// The vector store has no memory of its own past once its container restarts,
// so the orchestrator replays the surviving Redis entries on startup.
func (c *Client) Rebuild(ctx context.Context, entries []RebuildEntry) (int, error) {
	body, _ := json.Marshal(rebuildRequest{Entries: entries})
	var out rebuildResponse
	if err := c.post(ctx, "/rebuild", body, &out); err != nil {
		return 0, err
	}
	return out.Restored, nil
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
