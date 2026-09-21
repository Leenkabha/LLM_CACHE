// A minimal in-memory persistence plugin for the LLM Cache plugin platform.
//
// Contract (v1):
//
//	PUT    /v1/entries/{id}                 body: entry            -> 204
//	GET    /v1/entries/{id}                                        -> 200 entry | 404 not_found | 422 invalid_data
//	GET    /v1/entries?cursor=&limit=                              -> {"entries":[...],"next_cursor":""}
//	DELETE /v1/entries/{id}                                        -> 204 (unknown ids are fine)
//	GET    /v1/size                                                -> {"size":N}
//	POST   /v1/flush                                               -> 204
//	GET    /health                                                 -> 200
//
// An entry is {"id","prompt","reply","vector":[..],"created_at":"RFC3339"}. All
// fields must be stored: the platform rebuilds the vector store and the
// eviction policy from them.
//
// NOTE: this example keeps data in memory, so it is lost when the container
// restarts. A production persistence plugin must use a durable database.
package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"log"
	"math"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type entry struct {
	ID        string    `json:"id"`
	Prompt    string    `json:"prompt"`
	Reply     string    `json:"reply"`
	Vector    []float64 `json:"vector"`
	CreatedAt time.Time `json:"created_at"`
}

type store struct {
	mu   sync.RWMutex
	data map[string]entry
}

func validID(id string) bool {
	if id == "" || len(id) > 128 || id == "." || id == ".." {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '~' || c == '-') {
			return false
		}
	}
	return true
}

func main() {
	s := &store{data: map[string]entry{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("PUT /v1/entries/{id}", func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 8<<20)
		var e entry
		if err := json.NewDecoder(r.Body).Decode(&e); err != nil || e.ID != r.PathValue("id") || !validID(e.ID) {
			writeErr(w, http.StatusBadRequest, "invalid_request", "entry id must match the URL and be URL-safe")
			return
		}
		for _, x := range e.Vector {
			if math.IsNaN(x) || math.IsInf(x, 0) {
				writeErr(w, http.StatusBadRequest, "invalid_request", "non-finite vector value")
				return
			}
		}
		s.mu.Lock()
		s.data[e.ID] = e
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /v1/entries/{id}", func(w http.ResponseWriter, r *http.Request) {
		s.mu.RLock()
		e, ok := s.data[r.PathValue("id")]
		s.mu.RUnlock()
		if !ok {
			writeErr(w, http.StatusNotFound, "not_found", "no such entry")
			return
		}
		writeJSON(w, http.StatusOK, e)
	})
	mux.HandleFunc("GET /v1/entries", func(w http.ResponseWriter, r *http.Request) {
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		if limit < 1 || limit > 1000 {
			limit = 500
		}
		offset, _ := strconv.Atoi(r.URL.Query().Get("cursor")) // opaque to callers
		s.mu.RLock()
		all := make([]entry, 0, len(s.data))
		for _, e := range s.data {
			all = append(all, e)
		}
		s.mu.RUnlock()
		sort.Slice(all, func(i, j int) bool {
			if !all[i].CreatedAt.Equal(all[j].CreatedAt) {
				return all[i].CreatedAt.Before(all[j].CreatedAt)
			}
			return all[i].ID < all[j].ID
		})
		if offset < 0 || offset > len(all) {
			offset = len(all)
		}
		end := offset + limit
		next := ""
		if end < len(all) {
			next = strconv.Itoa(end)
		} else {
			end = len(all)
		}
		writeJSON(w, http.StatusOK, map[string]any{"entries": all[offset:end], "next_cursor": next})
	})
	mux.HandleFunc("DELETE /v1/entries/{id}", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		delete(s.data, r.PathValue("id"))
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /v1/size", func(w http.ResponseWriter, _ *http.Request) {
		s.mu.RLock()
		n := len(s.data)
		s.mu.RUnlock()
		writeJSON(w, http.StatusOK, map[string]int{"size": n})
	})
	mux.HandleFunc("POST /v1/flush", func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		s.data = map[string]entry{}
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
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
