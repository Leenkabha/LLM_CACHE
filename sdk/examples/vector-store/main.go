// A minimal in-memory vector-store plugin for the LLM Cache plugin platform.
//
// Contract (v1):
//
//	POST   /v1/search  {"vector":[..],"top_k":3,"threshold":0.25} -> {"matches":[{"id","distance"}]}
//	POST   /v1/upsert  {"vector":[..]}                            -> {"id":"..."}
//	DELETE /v1/entries/{id}                                        -> 200 | 404
//	GET    /v1/size                                                -> {"size":N}
//	POST   /v1/flush                                               -> 200
//	POST   /v1/rebuild {"entries":[{"id","vector"}]}               -> {"restored":N}   (REPLACES contents)
//	GET    /v1/info  (optional)                                    -> {"dim":8,"metric":"cosine"}
//	GET    /health                                                 -> {"status":"ok","dim":8,"metric":"cosine"}
//
// Lower distance is more similar; a hit requires distance <= threshold; results
// are best-to-worst and at most top_k. Brute force is fine for a demo; replace
// the store with a real index.
package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
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

var (
	dim    = envInt("CONFIG_DIM", 8)
	metric = getenv("CONFIG_METRIC", "cosine") // cosine | euclidean
)

type store struct {
	mu   sync.RWMutex
	vecs map[string][]float64
}

func (s *store) distance(a, b []float64) float64 {
	if metric == "euclidean" {
		var sum float64
		for i := range a {
			d := a[i] - b[i]
			sum += d * d
		}
		return math.Sqrt(sum)
	}
	var dot, na, nb float64
	for i := range a {
		dot += a[i] * b[i]
		na += a[i] * a[i]
		nb += b[i] * b[i]
	}
	if na == 0 || nb == 0 {
		return 1
	}
	d := 1 - dot/(math.Sqrt(na)*math.Sqrt(nb))
	if d < 0 && d > -1e-9 {
		d = 0
	}
	return d
}

func (s *store) search(v []float64, topK int, threshold float64) []map[string]any {
	s.mu.RLock()
	defer s.mu.RUnlock()
	type m struct {
		id string
		d  float64
	}
	var ms []m
	for id, x := range s.vecs {
		if d := s.distance(v, x); d <= threshold {
			ms = append(ms, m{id, d})
		}
	}
	sort.Slice(ms, func(i, j int) bool {
		if ms[i].d != ms[j].d {
			return ms[i].d < ms[j].d
		}
		return ms[i].id < ms[j].id
	})
	if len(ms) > topK {
		ms = ms[:topK]
	}
	out := make([]map[string]any, 0, len(ms))
	for _, x := range ms {
		out = append(out, map[string]any{"id": x.id, "distance": x.d})
	}
	return out
}

func checkVec(v []float64) bool {
	if len(v) != dim {
		return false
	}
	for _, x := range v {
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return false
		}
	}
	return true
}

func newID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func main() {
	s := &store{vecs: map[string][]float64{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "dim": dim, "metric": metric})
	})
	mux.HandleFunc("GET /v1/info", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"dim": dim, "metric": metric})
	})
	mux.HandleFunc("POST /v1/search", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Vector    []float64 `json:"vector"`
			TopK      int       `json:"top_k"`
			Threshold float64   `json:"threshold"`
		}
		if !decode(w, r, &req) || req.TopK < 1 || !checkVec(req.Vector) {
			writeErr(w, http.StatusBadRequest, "invalid_request", "vector must have the store dimension and top_k must be >= 1")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"matches": s.search(req.Vector, req.TopK, req.Threshold)})
	})
	mux.HandleFunc("POST /v1/upsert", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Vector []float64 `json:"vector"`
		}
		if !decode(w, r, &req) || !checkVec(req.Vector) {
			writeErr(w, http.StatusBadRequest, "invalid_request", "vector must have the store dimension")
			return
		}
		id := newID()
		s.mu.Lock()
		s.vecs[id] = append([]float64(nil), req.Vector...)
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]string{"id": id})
	})
	mux.HandleFunc("DELETE /v1/entries/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		s.mu.Lock()
		_, ok := s.vecs[id]
		delete(s.vecs, id)
		s.mu.Unlock()
		if !ok {
			writeErr(w, http.StatusNotFound, "not_found", "unknown id")
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
	})
	mux.HandleFunc("GET /v1/size", func(w http.ResponseWriter, _ *http.Request) {
		s.mu.RLock()
		n := len(s.vecs)
		s.mu.RUnlock()
		writeJSON(w, http.StatusOK, map[string]int{"size": n})
	})
	mux.HandleFunc("POST /v1/flush", func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		s.vecs = map[string][]float64{}
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]string{"status": "flushed"})
	})
	mux.HandleFunc("POST /v1/rebuild", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Entries []struct {
				ID     string    `json:"id"`
				Vector []float64 `json:"vector"`
			} `json:"entries"`
		}
		if !decode(w, r, &req) {
			writeErr(w, http.StatusBadRequest, "invalid_request", "entries required")
			return
		}
		next := make(map[string][]float64, len(req.Entries))
		for _, e := range req.Entries {
			if e.ID == "" || strings.ContainsAny(e.ID, "/?# ") || !checkVec(e.Vector) {
				writeErr(w, http.StatusBadRequest, "invalid_request", "bad entry")
				return
			}
			if _, dup := next[e.ID]; dup {
				writeErr(w, http.StatusBadRequest, "invalid_request", "duplicate id")
				return
			}
			next[e.ID] = append([]float64(nil), e.Vector...)
		}
		s.mu.Lock()
		s.vecs = next // replace, never merge
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]int{"restored": len(next)})
	})
	serve(mux)
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<20)
	return json.NewDecoder(r.Body).Decode(v) == nil
}

func envInt(k string, d int) int {
	if v, err := strconv.Atoi(os.Getenv(k)); err == nil {
		return v
	}
	return d
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
