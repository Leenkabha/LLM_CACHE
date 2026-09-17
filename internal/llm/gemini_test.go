package llm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func newTestGemini(baseURL string) *geminiBackend {
	return &geminiBackend{
		apiKey:  "test-key",
		model:   "test-model",
		baseURL: baseURL,
		http:    &http.Client{Timeout: 5 * time.Second},
	}
}

func TestGeminiRetriesOnRetryableStatusThenSucceeds(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"code":503,"message":"overloaded"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"answer"}]}}]}`))
	}))
	defer server.Close()

	reply, err := newTestGemini(server.URL).Complete(context.Background(), "hello")
	if err != nil || reply != "answer" {
		t.Fatalf("reply=%q err=%v", reply, err)
	}
	if calls != 3 {
		t.Fatalf("calls=%d, want 3 (2 retryable failures + 1 success)", calls)
	}
}

func TestGeminiDoesNotRetryNonRetryableStatus(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":400,"message":"bad request"}}`))
	}))
	defer server.Close()

	_, err := newTestGemini(server.URL).Complete(context.Background(), "hello")
	if err == nil {
		t.Fatal("expected error")
	}
	if calls != 1 {
		t.Fatalf("calls=%d, want 1 -- a 400 must not be retried, it will never succeed", calls)
	}
}

func TestGeminiRetriesOn429(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls < 2 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"answer"}]}}]}`))
	}))
	defer server.Close()

	reply, err := newTestGemini(server.URL).Complete(context.Background(), "hello")
	if err != nil || reply != "answer" {
		t.Fatalf("reply=%q err=%v", reply, err)
	}
	if calls != 2 {
		t.Fatalf("calls=%d, want 2", calls)
	}
}

// Regression test for a real failure mode hit during manual testing: a 429
// caused by an exhausted per-day free-tier quota was being retried 5 times
// with exponential backoff (~40-80s of real network round-trips) even though
// a daily quota cannot possibly recover within that window. It must fail
// fast instead, on the first attempt, so the fallback kicks in almost
// immediately rather than after a long, guaranteed-to-fail retry sequence.
func TestGeminiDoesNotRetryDailyQuotaExhaustion(t *testing.T) {
	var calls int
	dailyQuotaBody := `{
  "error": {
    "code": 429,
    "message": "You exceeded your current quota.",
    "status": "RESOURCE_EXHAUSTED",
    "details": [
      {
        "@type": "type.googleapis.com/google.rpc.QuotaFailure",
        "violations": [
          {
            "quotaMetric": "generativelanguage.googleapis.com/generate_content_free_tier_requests",
            "quotaId": "GenerateRequestsPerDayPerProjectPerModel-FreeTier",
            "quotaDimensions": {"model": "gemini-3.6-flash", "location": "global"},
            "quotaValue": "20"
          }
        ]
      },
      {"@type": "type.googleapis.com/google.rpc.RetryInfo", "retryDelay": "39s"}
    ]
  }
}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(dailyQuotaBody))
	}))
	defer server.Close()

	start := time.Now()
	_, err := newTestGemini(server.URL).Complete(context.Background(), "hello")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error")
	}
	if calls != 1 {
		t.Fatalf("calls=%d, want 1 -- a per-day quota error must not be retried", calls)
	}
	if elapsed > 100*time.Millisecond {
		t.Fatalf("elapsed=%v, want near-instant fail-fast (no backoff sleep)", elapsed)
	}
}

// A 429 WITHOUT the per-day quota marker (e.g. a short-term rate limit) must
// still be retried -- only the specific daily-quota case is fail-fast.
func TestGeminiStillRetriesPlain429(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls < 2 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"code":429,"message":"rate limited, try again shortly","status":"RESOURCE_EXHAUSTED"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"answer"}]}}]}`))
	}))
	defer server.Close()

	reply, err := newTestGemini(server.URL).Complete(context.Background(), "hello")
	if err != nil || reply != "answer" {
		t.Fatalf("reply=%q err=%v", reply, err)
	}
	if calls != 2 {
		t.Fatalf("calls=%d, want 2 -- a plain rate-limit 429 should still be retried", calls)
	}
}

func TestGeminiExhaustsRetriesOnPersistentOutage(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"code":503}}`))
	}))
	defer server.Close()

	_, err := newTestGemini(server.URL).Complete(context.Background(), "hello")
	if err == nil {
		t.Fatal("expected error after exhausting retries")
	}
	if calls != geminiMaxAttempts {
		t.Fatalf("calls=%d, want %d", calls, geminiMaxAttempts)
	}
}

func TestGeminiDoesNotRetryMalformedResponse(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write([]byte(`{"candidates":[]}`)) // 200 OK but no usable text
	}))
	defer server.Close()

	_, err := newTestGemini(server.URL).Complete(context.Background(), "hello")
	if err == nil {
		t.Fatal("expected error")
	}
	if calls != 1 {
		t.Fatalf("calls=%d, want 1 -- a response with no candidates will never gain one on retry", calls)
	}
}

func TestGeminiStopsRetryingOnContextCancellation(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := newTestGemini(server.URL).Complete(ctx, "hello")
	if err == nil {
		t.Fatal("expected error")
	}
	if calls >= geminiMaxAttempts {
		t.Fatalf("calls=%d, want fewer than %d -- the context deadline should stop retrying early", calls, geminiMaxAttempts)
	}
}

func TestGeminiMissingAPIKeyFailsFastWithoutAnyNetworkCall(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("must not make a network call with no API key")
	}))
	defer server.Close()

	g := newTestGemini(server.URL)
	g.apiKey = ""
	if _, err := g.Complete(context.Background(), "hello"); err == nil {
		t.Fatal("expected error")
	}
}

func TestGeminiUnreachableIsRetried(t *testing.T) {
	// Point at a closed port: connection refused is a transport error, which
	// must be classified as retryable (transient) just like a 503.
	g := newTestGemini("http://127.0.0.1:1")
	g.http.Timeout = 200 * time.Millisecond
	start := time.Now()
	_, err := g.Complete(context.Background(), "hello")
	if err == nil {
		t.Fatal("expected error")
	}
	// If retries happened, this took noticeably longer than one request.
	if elapsed := time.Since(start); elapsed < geminiInitialBackoff {
		t.Fatalf("elapsed=%v, expected at least one backoff sleep from a retry", elapsed)
	}
}
