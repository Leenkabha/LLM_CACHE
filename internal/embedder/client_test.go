package embedder

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestEmbedHTTP(t *testing.T) {
	for _, tc := range []struct {
		name, body   string
		status       int
		invalid      bool
		wantVecFirst float64
	}{
		{"ok", `{"vector":[0.1,0.2],"dim":2}`, 200, false, 0.1},
		{"error status", `{"detail":"boom"}`, 500, true, 0},
		{"not found", `not json at all`, 404, true, 0},
		{"bad json", `{`, 200, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "POST" || r.URL.Path != "/embed" {
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			vec, err := NewHTTP(server.URL).Embed(context.Background(), "hello")
			if (err != nil) != tc.invalid {
				t.Fatalf("error=%v, want invalid=%v", err, tc.invalid)
			}
			if !tc.invalid && (len(vec) == 0 || vec[0] != tc.wantVecFirst) {
				t.Fatalf("vec=%v, want first=%v", vec, tc.wantVecFirst)
			}
		})
	}
}

func TestEmbedHTTPUnreachable(t *testing.T) {
	if _, err := NewHTTP("http://127.0.0.1:1").Embed(context.Background(), "hello"); err == nil {
		t.Fatal("expected error for unreachable embedding service")
	}
}

func TestEmbedHTTPRequestBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Text string }
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			t.Fatal(err)
		}
		if in.Text != "What is virtual memory?" {
			t.Errorf("request text = %q", in.Text)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"vector":[1,0],"dim":2}`))
	}))
	defer server.Close()
	if _, err := NewHTTP(server.URL).Embed(context.Background(), "What is virtual memory?"); err != nil {
		t.Fatal(err)
	}
}

func TestHealthHTTP(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		invalid bool
	}{
		{"ok", 200, false},
		{"degraded", 503, true},
		{"error", 500, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" || r.URL.Path != "/health" {
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				}
				w.WriteHeader(tc.status)
			}))
			defer server.Close()
			err := NewHTTP(server.URL).Health(context.Background())
			if (err != nil) != tc.invalid {
				t.Fatalf("error=%v, want invalid=%v", err, tc.invalid)
			}
		})
	}
}

func TestHealthHTTPUnreachable(t *testing.T) {
	if err := NewHTTP("http://127.0.0.1:1").Health(context.Background()); err == nil {
		t.Fatal("expected error for unreachable embedding service")
	}
}

// The embedding service can take longer than instantaneous to respond under
// load; Embed must honor context cancellation/timeout rather than hanging.
func TestEmbedHTTPContextTimeout(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer server.Close()
	defer close(release)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := NewHTTP(server.URL).Embed(ctx, "hello"); err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestNewSelectsRegisteredBackend(t *testing.T) {
	if !registry.Has(BackendHTTP) {
		t.Fatal("default http backend not registered")
	}
}
