package llm

import (
	"bytes"
	"context"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// captureLog returns everything logged via the standard logger until cleanup.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(nil) })
	return &buf
}

func TestUsageLogsTokensAndRemainingQuota(t *testing.T) {
	buf := captureLog(t)
	tr := newUsageTracker(20, 1000)
	tr.recordSuccess("m", geminiUsage{PromptTokenCount: 10, CandidatesTokenCount: 40, ThoughtsTokenCount: 50, TotalTokenCount: 100})
	got := buf.String()
	for _, want := range []string{"prompt_tokens=10", "output_tokens=40", "thinking_tokens=50", "total_tokens=100",
		"requests=1", "tokens=100", "requests_left=19/20", "tokens_left=900/1000", "resets_in="} {
		if !strings.Contains(got, want) {
			t.Fatalf("log missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "LOW") {
		t.Fatalf("plenty left, must not warn LOW:\n%s", got)
	}
}

func TestUsageWarnsLowAndClampsAtZero(t *testing.T) {
	buf := captureLog(t)
	tr := newUsageTracker(5, 0)
	for i := 0; i < 5; i++ {
		tr.recordSuccess("m", geminiUsage{TotalTokenCount: 1})
	}
	if got := buf.String(); !strings.Contains(got, "requests_left=0/5 tokens_left=unlimited LOW") {
		t.Fatalf("want requests_left=0/5 ... LOW:\n%s", got)
	}
	buf.Reset()
	tr.recordSuccess("m", geminiUsage{TotalTokenCount: 1}) // over the limit
	if got := buf.String(); !strings.Contains(got, "requests_left=0/5") {
		t.Fatalf("remaining must clamp at 0:\n%s", got)
	}
}

func TestUsageUnknownLimitSaysSo(t *testing.T) {
	buf := captureLog(t)
	newUsageTracker(0, 0).recordSuccess("m", geminiUsage{TotalTokenCount: 7})
	if got := buf.String(); !strings.Contains(got, "requests_left=unknown") || strings.Contains(got, "LOW") {
		t.Fatalf("unexpected log:\n%s", got)
	}
}

func TestUsageResetsAtMidnightPacific(t *testing.T) {
	buf := captureLog(t)
	tr := newUsageTracker(10, 0)
	la := tr.loc
	clock := time.Date(2026, 9, 20, 23, 30, 0, 0, la)
	tr.now = func() time.Time { return clock }

	tr.recordSuccess("m", geminiUsage{TotalTokenCount: 5})
	if got := buf.String(); !strings.Contains(got, "requests=1 tokens=5") || !strings.Contains(got, "resets_in=30m0s") {
		t.Fatalf("day one:\n%s", got)
	}

	buf.Reset()
	clock = clock.Add(time.Hour) // 00:30 next Pacific day
	tr.recordSuccess("m", geminiUsage{TotalTokenCount: 3})
	if got := buf.String(); !strings.Contains(got, "requests=1 tokens=3") || !strings.Contains(got, "requests_left=9/10") {
		t.Fatalf("new day must start a fresh tally:\n%s", got)
	}
}

func TestUsageQuotaExhaustedLearnsLimit(t *testing.T) {
	buf := captureLog(t)
	tr := newUsageTracker(0, 0)
	body := []byte(`Quota exceeded for metric: generate_content_free_tier_requests, limit: 20, model: m ... GenerateRequestsPerDayPerProjectPerModel-FreeTier`)
	tr.recordQuotaExhausted("m", body)
	got := buf.String()
	if !strings.Contains(got, "quota EXHAUSTED") || !strings.Contains(got, "requests_left=0/20") {
		t.Fatalf("want exhausted with learned limit 20:\n%s", got)
	}
}

func TestNilUsageTrackerIsSafe(t *testing.T) {
	var tr *usageTracker
	tr.recordSuccess("m", geminiUsage{TotalTokenCount: 1})
	tr.recordQuotaExhausted("m", nil)
}

func TestGeminiLogsUsageFromResponse(t *testing.T) {
	buf := captureLog(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"hi"}]}}],
			"usageMetadata":{"promptTokenCount":11,"candidatesTokenCount":22,"thoughtsTokenCount":3,"totalTokenCount":36}}`))
	}))
	defer server.Close()

	g := newTestGemini(server.URL)
	g.usage = newUsageTracker(20, 0)
	if _, err := g.Complete(context.Background(), "hello"); err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); !strings.Contains(got, "total_tokens=36") || !strings.Contains(got, "requests_left=19/20") {
		t.Fatalf("usage not logged from response:\n%s", got)
	}
}

func TestGeminiLogsQuotaExhaustionOnDailyQuota429(t *testing.T) {
	buf := captureLog(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"limit: 20, model: m","details":[{"violations":[{"quotaId":"GenerateRequestsPerDayPerProjectPerModel-FreeTier"}]}]}}`))
	}))
	defer server.Close()

	g := newTestGemini(server.URL)
	g.usage = newUsageTracker(0, 0)
	if _, err := g.Complete(context.Background(), "hello"); err == nil {
		t.Fatal("expected error")
	}
	if got := buf.String(); strings.Count(got, "quota EXHAUSTED") != 1 || !strings.Contains(got, "requests_left=0/20") {
		t.Fatalf("want exactly one exhausted line with limit 20:\n%s", got)
	}
}

func TestUsageSnapshotReportsLeftOnlyWhenLimitsSet(t *testing.T) {
	captureLog(t)
	tr := newUsageTracker(20, 0)
	tr.recordSuccess("m", geminiUsage{TotalTokenCount: 30})
	s := tr.snapshot("m")
	if s.Requests != 1 || s.Tokens != 30 || s.RequestsLeft == nil || *s.RequestsLeft != 19 {
		t.Fatalf("snapshot=%+v", s)
	}
	if s.TokensLeft != nil {
		t.Fatalf("no token budget configured, tokens_left must be nil, got %d", *s.TokensLeft)
	}
	if s.ResetsInSeconds <= 0 || s.ResetsInSeconds > 24*3600 {
		t.Fatalf("resets_in_seconds=%d", s.ResetsInSeconds)
	}
}

func TestUsageSnapshotExhaustedForcesZeroLeft(t *testing.T) {
	captureLog(t)
	tr := newUsageTracker(0, 0)
	tr.recordQuotaExhausted("m", []byte("limit: 20"))
	s := tr.snapshot("m")
	if !s.Exhausted || s.RequestsLeft == nil || *s.RequestsLeft != 0 || s.RequestLimit != 20 {
		t.Fatalf("snapshot=%+v", s)
	}
}

func TestGeminiAndFallbackExposeUsage(t *testing.T) {
	g := newTestGemini("http://unused")
	if _, ok := g.Usage(); ok {
		t.Fatal("no tracker configured: Usage must report false")
	}
	g.usage = newUsageTracker(5, 0)
	if _, ok := NewFallback(g, &stubBackend{}).(UsageReporter).Usage(); !ok {
		t.Fatal("fallback must expose the primary's usage")
	}
	if _, ok := NewFallback(&stubBackend{}, g).(UsageReporter).Usage(); ok {
		t.Fatal("a primary without usage must report false")
	}
}
