// A minimal, deterministic LLM plugin for the LLM Cache plugin platform.
//
// Contract (v1):
//
//	POST /v1/complete {"model":"m","prompt":"p"} -> 200 {"reply":"text"}
//	GET  /v1/usage    (optional)                 -> 200 usage JSON
//	GET  /health                                 -> 200
//
// Replace complete() with a call to your real model. This example never calls a
// network service, so it is safe to run in tests.
package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

var requests atomic.Int64

// complete is the only function you need to replace.
func complete(model, prompt string) string {
	tag := "ok"
	// API_TOKEN is an optional secret injected at runtime. Never echo it.
	if os.Getenv("API_TOKEN") != "" {
		tag = "secret-configured"
	}
	return "[sdk-llm " + model + " " + tag + "] echo: " + strings.TrimSpace(prompt)
}

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("POST /v1/complete", func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		var req struct {
			Model  string `json:"model"`
			Prompt string `json:"prompt"`
		}
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil || strings.TrimSpace(req.Prompt) == "" {
			writeErr(w, http.StatusBadRequest, "invalid_request", "prompt is required")
			return
		}
		requests.Add(1)
		writeJSON(w, http.StatusOK, map[string]string{"reply": complete(req.Model, req.Prompt)})
	})
	mux.HandleFunc("GET /v1/usage", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"model": "sdk-llm", "requests": requests.Load(), "tokens": 0, "exhausted": false, "resets_in_seconds": 0})
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
