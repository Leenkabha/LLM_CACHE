package orchestrator

import (
	"crypto/subtle"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// guard protects a publicly reachable orchestrator. It is opt-in through
// configuration so local development keeps working unchanged:
//   - ADMIN_TOKEN set: POST /flush and POST /policy require
//     "Authorization: Bearer <token>".
//   - RATE_LIMIT_PER_MIN > 0: POST /query is limited per client IP.
type guard struct {
	adminToken string
	limiter    *rateLimiter
	trustProxy bool
	next       http.Handler
}

func newGuard(next http.Handler, adminToken string, ratePerMin int, trustProxy bool) http.Handler {
	if adminToken == "" && ratePerMin <= 0 {
		return next
	}
	g := &guard{adminToken: adminToken, trustProxy: trustProxy, next: next}
	if ratePerMin > 0 {
		g.limiter = newRateLimiter(ratePerMin, time.Minute)
	}
	return g
}

func (g *guard) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if g.adminToken != "" && r.Method == http.MethodPost && (r.URL.Path == "/flush" || r.URL.Path == "/policy") {
		if !g.validToken(r.Header.Get("Authorization")) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeError(w, http.StatusUnauthorized, "admin token required")
			return
		}
	}
	if g.limiter != nil && r.Method == http.MethodPost && r.URL.Path == "/query" {
		if !g.limiter.allow(g.clientIP(r)) {
			w.Header().Set("Retry-After", "60")
			writeError(w, http.StatusTooManyRequests, "rate limit exceeded, try again shortly")
			return
		}
	}
	g.next.ServeHTTP(w, r)
}

func (g *guard) validToken(header string) bool {
	token, ok := strings.CutPrefix(header, "Bearer ")
	if !ok {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(token), []byte(g.adminToken)) == 1
}

// clientIP returns the caller's address. Behind a reverse proxy the proxy's
// own address is what RemoteAddr shows, so with TRUST_PROXY the first
// X-Forwarded-For entry is used instead. Only enable that when the orchestrator
// is not directly reachable by clients, or the header can be spoofed.
func (g *guard) clientIP(r *http.Request) string {
	if g.trustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			first, _, _ := strings.Cut(xff, ",")
			if ip := strings.TrimSpace(first); ip != "" {
				return ip
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// rateLimiter is a fixed-window per-key counter.
type rateLimiter struct {
	mu      sync.Mutex
	limit   int
	window  time.Duration
	buckets map[string]*bucket
	now     func() time.Time
}

type bucket struct {
	start time.Time
	count int
}

func newRateLimiter(limit int, window time.Duration) *rateLimiter {
	return &rateLimiter{limit: limit, window: window, buckets: map[string]*bucket{}, now: time.Now}
}

func (l *rateLimiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if len(l.buckets) > 10000 {
		for k, b := range l.buckets {
			if now.Sub(b.start) >= l.window {
				delete(l.buckets, k)
			}
		}
	}
	b, ok := l.buckets[key]
	if !ok || now.Sub(b.start) >= l.window {
		l.buckets[key] = &bucket{start: now, count: 1}
		return true
	}
	if b.count >= l.limit {
		return false
	}
	b.count++
	return true
}
