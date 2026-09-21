// Package protocol implements the versioned (v1) remote plugin contracts and the
// adapters that make a remote plugin look like the in-process Go interfaces the
// orchestrator already uses.
//
// All remote calls go through Client, which bounds every request and response,
// never follows redirects (when built on safehttp) and never puts a plugin's
// response body into an error message, so a hostile or buggy plugin cannot push
// text into logs or API responses beyond a short, scrubbed status description.
package protocol

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/leenkabha/llm_cache/internal/plugins/safehttp"
	"github.com/leenkabha/llm_cache/internal/plugins/secrets"
)

// Default limits.
const (
	DefaultTimeout     = 10 * time.Second
	DefaultMaxResponse = 8 << 20
	MaxReplyBytes      = 1 << 20
)

// idRE is what an entry ID may look like: non-blank and safe as one URL path
// segment without escaping.
var idRE = regexp.MustCompile(`^[A-Za-z0-9._~-]{1,128}$`)

// ValidID reports whether id satisfies the plugin ID rule.
func ValidID(id string) bool { return idRE.MatchString(id) && id != "." && id != ".." }

// Client talks to one remote plugin.
type Client struct {
	Base        string       // scheme://host[:port][/prefix], no trailing slash
	HTTP        *http.Client // safehttp client for hosted endpoints
	Token       string       // optional bearer credential
	MaxResponse int64
	Timeout     time.Duration
}

// NewClient validates base and builds a Client using the given HTTP client.
func NewClient(base string, hc *http.Client, token string) (*Client, error) {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		return nil, errors.New("invalid plugin base URL")
	}
	return &Client{Base: strings.TrimRight(base, "/"), HTTP: hc, Token: token, MaxResponse: DefaultMaxResponse, Timeout: DefaultTimeout}, nil
}

// StatusError is a non-success reply from the plugin.
type StatusError struct {
	Status  int
	Code    string // machine-readable code from {"error":{"code":...}} when present
	Message string // short, scrubbed
}

func (e *StatusError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("plugin returned HTTP %d (%s)", e.Status, e.Code)
	}
	return fmt.Sprintf("plugin returned HTTP %d", e.Status)
}

// Sentinel errors for protocol violations by the plugin.
var (
	ErrMalformed = errors.New("plugin returned a malformed response")
	ErrTooLarge  = errors.New("plugin response exceeded the size limit")
)

// Do sends one request. body may be nil; out may be nil. It returns the status
// code; a non-2xx status is returned together with a *StatusError.
func (c *Client) Do(ctx context.Context, method, path string, body, out any) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	var rd *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, fmt.Errorf("encode request: %w", err)
		}
		rd = bytes.NewReader(b)
	}
	var req *http.Request
	var err error
	if rd != nil {
		req, err = http.NewRequestWithContext(ctx, method, c.Base+path, rd)
	} else {
		req, err = http.NewRequestWithContext(ctx, method, c.Base+path, nil)
	}
	if err != nil {
		return 0, errors.New("could not build plugin request")
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}

	timeout := c.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req = req.WithContext(cctx)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return 0, fmt.Errorf("plugin request timed out after %s", timeout)
		}
		// Transport errors can embed the URL; report only that the call failed.
		return 0, errors.New("plugin unreachable or refused the connection")
	}
	defer resp.Body.Close()

	max := c.MaxResponse
	if max <= 0 {
		max = DefaultMaxResponse
	}
	data, err := safehttp.ReadLimited(resp.Body, max)
	if err != nil {
		if errors.Is(err, safehttp.ErrTooLarge) {
			return resp.StatusCode, ErrTooLarge
		}
		if ctx.Err() != nil {
			return resp.StatusCode, ctx.Err()
		}
		return resp.StatusCode, errors.New("reading plugin response failed")
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return resp.StatusCode, parseStatusError(resp.StatusCode, data)
	}
	if out != nil && len(bytes.TrimSpace(data)) > 0 {
		if err := decodeStrict(data, out); err != nil {
			return resp.StatusCode, ErrMalformed
		}
	} else if out != nil && resp.StatusCode != http.StatusNoContent {
		return resp.StatusCode, ErrMalformed
	}
	return resp.StatusCode, nil
}

func decodeStrict(data []byte, out any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(out); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("trailing data")
	}
	return nil
}

// parseStatusError extracts a short machine code from the common error shapes
// {"error":{"code":"x","message":"y"}} and {"error":"y"}. Only a scrubbed,
// truncated message is kept.
func parseStatusError(status int, data []byte) error {
	se := &StatusError{Status: status}
	var env struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(data, &env) == nil && len(env.Error) > 0 {
		var obj struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		if json.Unmarshal(env.Error, &obj) == nil && (obj.Code != "" || obj.Message != "") {
			se.Code = sanitizeCode(obj.Code)
			se.Message = short(obj.Message)
		} else {
			var s string
			if json.Unmarshal(env.Error, &s) == nil {
				se.Message = short(s)
			}
		}
	}
	return se
}

var codeRE = regexp.MustCompile(`[^a-z0-9_]`)

func sanitizeCode(c string) string {
	c = codeRE.ReplaceAllString(strings.ToLower(c), "")
	if len(c) > 40 {
		c = c[:40]
	}
	return c
}

func short(s string) string {
	s = secrets.Scrub(s)
	if len(s) > 160 {
		s = s[:160]
	}
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
}

// Health calls GET /health and requires 200.
func (c *Client) Health(ctx context.Context) error {
	_, err := c.Do(ctx, http.MethodGet, "/health", nil, nil)
	return err
}

// HealthInfo is the optional body a /health reply may carry.
type HealthInfo struct {
	Status string `json:"status"`
	Dim    int    `json:"dim,omitempty"`
	Metric string `json:"metric,omitempty"`
}

// HealthDetail calls GET /health and decodes the optional extra fields.
func (c *Client) HealthDetail(ctx context.Context) (HealthInfo, error) {
	var h HealthInfo
	body := json.RawMessage{}
	if _, err := c.Do(ctx, http.MethodGet, "/health", nil, &body); err != nil && !errors.Is(err, ErrMalformed) {
		return h, err
	}
	_ = json.Unmarshal(body, &h) // extra fields are optional
	return h, nil
}
