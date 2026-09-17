package llm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/leenkabha/llm_cache/internal/config"
	"github.com/leenkabha/llm_cache/internal/contracttest"
)

func TestExampleHTTPContractAndRegistration(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/complete" || r.Header.Get("Authorization") != "Bearer test-token" || r.Header.Get("Content-Type") != "application/json" {
			t.Error("incorrect request metadata")
		}
		var in struct{ Model, Prompt string }
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			t.Error(err)
		}
		if in.Model != "demo" || in.Prompt != "hello" {
			t.Errorf("request=%+v", in)
		}
		_, _ = w.Write([]byte(`{"reply":"answer"}`))
	}))
	defer server.Close()
	t.Setenv("EXAMPLE_LLM_URL", server.URL+"/complete")
	t.Setenv("EXAMPLE_LLM_MODEL", "demo")
	t.Setenv("EXAMPLE_LLM_TOKEN", "test-token")
	t.Setenv("EXAMPLE_LLM_TIMEOUT", "2s")
	contracttest.LLMContract(t, func() contracttest.LLM {
		b, err := New(config.Config{LLMMode: ModeExampleHTTP})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}, "hello", "answer")
}

func TestExampleHTTPRejectsInvalidConfiguration(t *testing.T) {
	for _, tc := range []struct{ url, model, timeout string }{
		{"", "demo", ""}, {"file:///tmp/model", "demo", ""}, {"http://", "demo", ""},
		{"https://user:secret@example.com", "demo", ""}, {"https://example.com/#fragment", "demo", ""},
		{"http://localhost:8090", "", ""}, {"http://localhost:8090", "demo", "0s"},
		{"http://localhost:8090", "demo", "-1s"}, {"http://localhost:8090", "demo", "invalid"},
	} {
		if _, err := newExampleHTTP(tc.url, tc.model, "", tc.timeout); err == nil {
			t.Errorf("accepted invalid configuration %+v", tc)
		}
	}
}

func TestExampleHTTPErrors(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
	}{
		{"provider error", "private provider details and secret", 500},
		{"unauthorized", "secret", 401}, {"rate limit", "secret", 429},
		{"invalid json", "not json", 200}, {"empty reply", `{"reply":" "}`, 200},
		{"missing reply", `{}`, 200}, {"too large", strings.Repeat("x", exampleMaxReplyBytes+1), 200},
		{"trailing data", `{"reply":"ok"}garbage`, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			b, err := newExampleHTTP(server.URL, "demo", "secret", "")
			if err != nil {
				t.Fatal(err)
			}
			reply, err := b.Complete(context.Background(), "hello")
			if err == nil || reply != "" {
				t.Fatalf("reply=%q error=%v", reply, err)
			}
			if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "private") {
				t.Fatal("error exposed sensitive content")
			}
		})
	}
}

func TestExampleHTTPDoesNotFollowRedirects(t *testing.T) {
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("followed redirect") }))
	defer destination.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, destination.URL, 307) }))
	defer source.Close()
	b, err := newExampleHTTP(source.URL, "demo", "secret", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Complete(context.Background(), "hello"); err == nil {
		t.Fatal("redirect accepted")
	}
}

func TestExampleHTTPInFlightCancellation(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(entered); <-release }))
	defer server.Close()
	defer close(release)
	b, err := newExampleHTTP(server.URL, "demo", "", "")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { _, err := b.Complete(ctx, "hello"); result <- err }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("request did not reach server")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation did not stop request")
	}
}

func TestExampleHTTPClientTimeout(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-release }))
	defer server.Close()
	defer close(release)
	b, err := newExampleHTTP(server.URL, "demo", "", "50ms")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err = b.Complete(ctx, "hello")
	if err == nil || ctx.Err() != nil {
		t.Fatalf("client timeout failed: error=%v context=%v", err, ctx.Err())
	}
}
