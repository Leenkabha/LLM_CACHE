// Package ctlapi defines the wire contract between the orchestrator's plugin
// manager and the internal plugin controller, and the HTTP client the manager
// uses. It has no dependency on either side, so a Kubernetes controller can
// implement the same API.
//
// Every request carries "Authorization: Bearer <PLUGIN_CONTROLLER_TOKEN>". The
// controller is never exposed publicly; only the orchestrator talks to it.
package ctlapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/leenkabha/llm_cache/internal/plugins"
	"github.com/leenkabha/llm_cache/internal/plugins/registry"
	"github.com/leenkabha/llm_cache/internal/plugins/secrets"
)

// PrepareRequest asks the controller to make a plugin runnable.
type PrepareRequest struct {
	Mode     plugins.Mode `json:"mode"` // image | repository
	Type     plugins.Type `json:"type"`
	Image    string       `json:"image,omitempty"`
	Repo     string       `json:"repo,omitempty"`
	Revision string       `json:"revision,omitempty"`
	// Manifest is the administrator-supplied plugin.yaml for an image that does
	// not carry one. Repositories always use the manifest in the repository.
	Manifest string `json:"manifest,omitempty"`
	// DryRun resolves and validates without building or pulling large layers:
	// it returns the manifest and commit so the UI can show the form.
	DryRun bool `json:"dry_run,omitempty"`
	// PluginID names the build for log correlation.
	PluginID string `json:"plugin_id,omitempty"`
}

// PrepareResult is what a prepared plugin looks like.
type PrepareResult struct {
	Digest     string               `json:"digest,omitempty"` // immutable: sha256:... (manifest digest or image ID)
	ImageSize  int64                `json:"image_size_bytes,omitempty"`
	Manifest   string               `json:"manifest"` // plugin.yaml text
	Commit     string               `json:"commit,omitempty"`
	Scan       *registry.ScanReport `json:"scan,omitempty"`
	Provenance *registry.Provenance `json:"provenance,omitempty"`
	Log        []string             `json:"log,omitempty"` // sanitised
}

// StartRequest starts an instance. Env is injected only into the container's
// environment at start; the controller never persists it.
type StartRequest struct {
	InstanceID string            `json:"instance_id"`
	Type       plugins.Type      `json:"type"`
	Digest     string            `json:"digest"`
	Manifest   string            `json:"manifest"`
	Env        map[string]string `json:"env,omitempty"`
	HealthWait time.Duration     `json:"health_wait_ns,omitempty"`
}

// Instance is a running plugin as the orchestrator reaches it.
type Instance struct {
	ID      string `json:"id"`
	BaseURL string `json:"base_url"` // filled in by the client: the controller's proxy URL
}

// Error is an error reply from the controller.
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string { return e.Message }

// Client implements manager.Controller over HTTP.
type Client struct {
	Base  string
	Token string
	HTTP  *http.Client
}

// NewClient builds a client for the controller at base.
func NewClient(base, token string) *Client {
	return &Client{
		Base: strings.TrimRight(base, "/"), Token: token,
		HTTP: &http.Client{Timeout: 30 * time.Minute, Transport: &http.Transport{Proxy: nil, MaxIdleConnsPerHost: 8}},
	}
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.Base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("plugin controller unreachable")
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return errors.New("reading controller response failed")
	}
	if resp.StatusCode/100 != 2 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(data, &e)
		msg := e.Error
		if msg == "" {
			msg = fmt.Sprintf("controller returned HTTP %d", resp.StatusCode)
		}
		return &Error{Status: resp.StatusCode, Message: secrets.Scrub(msg)}
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return errors.New("controller returned an invalid response")
		}
	}
	return nil
}

func (c *Client) Prepare(ctx context.Context, req PrepareRequest) (*PrepareResult, error) {
	var out PrepareResult
	if err := c.do(ctx, http.MethodPost, "/v1/prepare", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) Start(ctx context.Context, req StartRequest) (*Instance, error) {
	var out Instance
	if err := c.do(ctx, http.MethodPost, "/v1/instances", req, &out); err != nil {
		return nil, err
	}
	out.BaseURL = c.Base + "/v1/proxy/" + url.PathEscape(out.ID)
	return &out, nil
}

func (c *Client) Stop(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/v1/instances/"+url.PathEscape(id), nil, nil)
}

func (c *Client) Logs(ctx context.Context, id string, tail int) ([]string, error) {
	var out struct {
		Lines []string `json:"lines"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/instances/"+url.PathEscape(id)+"/logs?tail="+strconv.Itoa(tail), nil, &out); err != nil {
		return nil, err
	}
	return out.Lines, nil
}

func (c *Client) Health(ctx context.Context) error {
	return c.do(ctx, http.MethodGet, "/health", nil, nil)
}
