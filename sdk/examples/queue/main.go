// A minimal in-memory, lease-based queue plugin for the LLM Cache plugin platform.
//
// Contract (v1):
//
//	POST /v1/jobs                {"prompt","reply","vector":[..]}                  -> 201 {"id":"..."}
//	POST /v1/jobs/lease          {"max_jobs":1,"visibility_timeout_seconds":60,"wait_seconds":2}
//	                                                                               -> 200 {"jobs":[{"id","prompt","reply","vector","lease_token","attempt"}]} | 204
//	POST /v1/jobs/{id}/ack       {"lease_token":"t"}                               -> 200 | 409
//	POST /v1/jobs/{id}/nack      {"lease_token":"t"}                               -> 200 | 409
//	GET  /v1/jobs/stats                                                            -> {"pending":N,"leased":M}
//	GET  /health
//
// A leased job is hidden until the visibility timeout expires, then it is
// delivered again. Only ack removes a job. Enqueue returns 201 only once the job
// is stored -- replace the in-memory slice with a durable store for production.
package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

type job struct {
	ID       string    `json:"id"`
	Prompt   string    `json:"prompt"`
	Reply    string    `json:"reply"`
	Vector   []float64 `json:"vector"`
	Token    string    `json:"lease_token,omitempty"`
	Attempt  int       `json:"attempt"`
	visible  time.Time
	leasedTo time.Time
}

type queue struct {
	mu   sync.Mutex
	jobs []*job // insertion order
}

func randID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// lease returns the oldest job that is not currently leased.
func (q *queue) lease(vis time.Duration) *job {
	q.mu.Lock()
	defer q.mu.Unlock()
	now := time.Now()
	for _, j := range q.jobs {
		if now.Before(j.visible) {
			continue
		}
		j.Attempt++
		j.Token = randID()
		j.visible = now.Add(vis)
		c := *j
		return &c
	}
	return nil
}

func (q *queue) settle(id, token string, remove bool) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i, j := range q.jobs {
		if j.ID != id {
			continue
		}
		if j.Token == "" || subtle.ConstantTimeCompare([]byte(token), []byte(j.Token)) != 1 {
			return http.StatusConflict // lease expired and was reissued, or never leased
		}
		if remove {
			q.jobs = append(q.jobs[:i], q.jobs[i+1:]...)
		} else {
			j.Token, j.visible = "", time.Time{} // visible again immediately
		}
		return http.StatusOK
	}
	return http.StatusNotFound
}

func main() {
	q := &queue{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("POST /v1/jobs", func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 8<<20)
		var j job
		if err := json.NewDecoder(r.Body).Decode(&j); err != nil || j.Prompt == "" || j.Reply == "" {
			writeErr(w, http.StatusBadRequest, "invalid_request", "prompt and reply are required")
			return
		}
		j.ID, j.Token, j.Attempt = randID(), "", 0
		q.mu.Lock()
		q.jobs = append(q.jobs, &j)
		q.mu.Unlock()
		writeJSON(w, http.StatusCreated, map[string]string{"id": j.ID})
	})
	mux.HandleFunc("POST /v1/jobs/lease", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			MaxJobs int `json:"max_jobs"`
			Vis     int `json:"visibility_timeout_seconds"`
			Wait    int `json:"wait_seconds"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Vis < 1 || req.Vis > 3600 {
			req.Vis = 60
		}
		if req.Wait < 0 || req.Wait > 20 {
			req.Wait = 0
		}
		deadline := time.Now().Add(time.Duration(req.Wait) * time.Second)
		for {
			if j := q.lease(time.Duration(req.Vis) * time.Second); j != nil {
				writeJSON(w, http.StatusOK, map[string]any{"jobs": []*job{j}})
				return
			}
			if time.Now().After(deadline) || r.Context().Err() != nil {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			select {
			case <-time.After(25 * time.Millisecond):
			case <-r.Context().Done():
			}
		}
	})
	settle := func(remove bool) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			var req struct {
				Token string `json:"lease_token"`
			}
			if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req) != nil || req.Token == "" {
				writeErr(w, http.StatusBadRequest, "invalid_request", "lease_token required")
				return
			}
			switch code := q.settle(r.PathValue("id"), req.Token, remove); code {
			case http.StatusOK:
				writeJSON(w, code, map[string]string{"status": "ok"})
			case http.StatusConflict:
				writeErr(w, code, "lease_lost", "the lease expired or was reissued")
			default:
				writeErr(w, code, "not_found", "unknown job")
			}
		}
	}
	mux.HandleFunc("POST /v1/jobs/{id}/ack", settle(true))
	mux.HandleFunc("POST /v1/jobs/{id}/nack", settle(false))
	mux.HandleFunc("GET /v1/jobs/stats", func(w http.ResponseWriter, _ *http.Request) {
		q.mu.Lock()
		defer q.mu.Unlock()
		pending, leased := 0, 0
		for _, j := range q.jobs {
			if time.Now().Before(j.visible) {
				leased++
			} else {
				pending++
			}
		}
		writeJSON(w, http.StatusOK, map[string]int{"pending": pending, "leased": leased})
	})
	serve(mux)
}

// ---- boilerplate shared by every example -------------------------------------

func serve(h http.Handler) {
	addr := ":" + getenv("PORT", "8080")
	srv := &http.Server{Addr: addr, Handler: withAuth(h), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		log.Printf("plugin listening on %s", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}

// withAuth enforces PLUGIN_AUTH_TOKEN (set by the platform) on everything except /health.
func withAuth(next http.Handler) http.Handler {
	token := os.Getenv("PLUGIN_AUTH_TOKEN")
	if token == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
				writeErr(w, http.StatusUnauthorized, "unauthorized", "bad credentials")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func getenv(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, errCode, msg string) {
	writeJSON(w, code, map[string]any{"error": map[string]string{"code": errCode, "message": msg}})
}
