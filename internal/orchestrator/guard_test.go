package orchestrator

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

var okHandler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
})

func do(h http.Handler, method, path, auth, remote, xff string) int {
	req := httptest.NewRequest(method, path, strings.NewReader("{}"))
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	if remote != "" {
		req.RemoteAddr = remote
	}
	if xff != "" {
		req.Header.Set("X-Forwarded-For", xff)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

func TestGuardDisabledIsPassthrough(t *testing.T) {
	h := newGuard(okHandler, "", 0, false)
	if code := do(h, http.MethodPost, "/flush", "", "", ""); code != http.StatusOK {
		t.Fatalf("flush with guard disabled = %d, want 200", code)
	}
}

func TestGuardAdminToken(t *testing.T) {
	h := newGuard(okHandler, "s3cret", 0, false)
	cases := []struct {
		name, method, path, auth string
		want                     int
	}{
		{"flush without token", http.MethodPost, "/flush", "", http.StatusUnauthorized},
		{"flush wrong token", http.MethodPost, "/flush", "Bearer nope", http.StatusUnauthorized},
		{"flush non-bearer", http.MethodPost, "/flush", "s3cret", http.StatusUnauthorized},
		{"flush right token", http.MethodPost, "/flush", "Bearer s3cret", http.StatusOK},
		{"policy without token", http.MethodPost, "/policy", "", http.StatusUnauthorized},
		{"policy right token", http.MethodPost, "/policy", "Bearer s3cret", http.StatusOK},
		{"query stays public", http.MethodPost, "/query", "", http.StatusOK},
		{"stats stays public", http.MethodGet, "/stats", "", http.StatusOK},
		{"health stays public", http.MethodGet, "/health", "", http.StatusOK},
	}
	for _, c := range cases {
		if got := do(h, c.method, c.path, c.auth, "", ""); got != c.want {
			t.Errorf("%s: got %d, want %d", c.name, got, c.want)
		}
	}
}

func TestGuardRateLimitPerIP(t *testing.T) {
	h := newGuard(okHandler, "", 2, false)
	for i := 0; i < 2; i++ {
		if code := do(h, http.MethodPost, "/query", "", "1.2.3.4:5000", ""); code != http.StatusOK {
			t.Fatalf("request %d = %d, want 200", i+1, code)
		}
	}
	if code := do(h, http.MethodPost, "/query", "", "1.2.3.4:5001", ""); code != http.StatusTooManyRequests {
		t.Fatalf("third request = %d, want 429", code)
	}
	if code := do(h, http.MethodPost, "/query", "", "5.6.7.8:5000", ""); code != http.StatusOK {
		t.Fatalf("different IP = %d, want 200", code)
	}
	if code := do(h, http.MethodGet, "/health", "", "1.2.3.4:5002", ""); code != http.StatusOK {
		t.Fatalf("health must not be rate limited, got %d", code)
	}
}

func TestGuardTrustProxyUsesForwardedFor(t *testing.T) {
	h := newGuard(okHandler, "", 1, true)
	if code := do(h, http.MethodPost, "/query", "", "10.0.0.1:1", "9.9.9.9"); code != http.StatusOK {
		t.Fatalf("first = %d, want 200", code)
	}
	// Same proxy address, different real client: must not share a bucket.
	if code := do(h, http.MethodPost, "/query", "", "10.0.0.1:1", "8.8.8.8"); code != http.StatusOK {
		t.Fatalf("other client behind same proxy = %d, want 200", code)
	}
	if code := do(h, http.MethodPost, "/query", "", "10.0.0.1:1", "9.9.9.9, 10.0.0.1"); code != http.StatusTooManyRequests {
		t.Fatalf("repeat client = %d, want 429", code)
	}
}

func TestGuardIgnoresForwardedForWithoutTrustProxy(t *testing.T) {
	h := newGuard(okHandler, "", 1, false)
	do(h, http.MethodPost, "/query", "", "1.1.1.1:1", "a")
	if code := do(h, http.MethodPost, "/query", "", "1.1.1.1:2", "b"); code != http.StatusTooManyRequests {
		t.Fatalf("spoofed XFF must not bypass the limit, got %d", code)
	}
}

func TestRateLimiterWindowResets(t *testing.T) {
	l := newRateLimiter(1, time.Minute)
	now := time.Unix(1000, 0)
	l.now = func() time.Time { return now }
	if !l.allow("k") || l.allow("k") {
		t.Fatal("expected first allowed, second denied")
	}
	now = now.Add(time.Minute)
	if !l.allow("k") {
		t.Fatal("expected allowance after window elapsed")
	}
}
