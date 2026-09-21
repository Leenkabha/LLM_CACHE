// A minimal, deterministic embedder plugin for the LLM Cache plugin platform.
//
// Contract (v1):
//
//	POST /v1/embed      {"text":"hello"} -> 200 {"vector":[...],"dim":8,"model":"name"}
//	GET  /v1/model-info                  -> 200 {"name":"name","dim":8}
//	GET  /health                         -> 200
//
// Vectors are unit length (cosine friendly). Replace embed() with your model.
// This example hashes word tokens into buckets, so texts that share words are
// close in cosine distance. It is a demo, not a semantic model.
package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"hash/fnv"
	"log"
	"math"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"
)

var dim = envInt("CONFIG_DIM", 8)

func modelName() string {
	return getenv("CONFIG_MODEL_NAME", "sdk-hash-embedder") + "-d" + strconv.Itoa(dim)
}

// embed is the only function you need to replace. It must return exactly dim
// finite numbers, normalised to unit length.
func embed(text string) []float64 {
	v := make([]float64, dim)
	tokens := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	if len(tokens) == 0 {
		tokens = []string{"<empty>"}
	}
	for _, t := range tokens {
		h := fnv.New64a()
		_, _ = h.Write([]byte(t))
		sum := h.Sum64()
		v[int(sum%uint64(dim))] += 1
		if sum&(1<<40) != 0 { // a second, sign-varying bucket spreads the mass
			v[int((sum>>20)%uint64(dim))] -= 0.5
		}
	}
	var norm float64
	for _, x := range v {
		norm += x * x
	}
	if norm == 0 {
		v[0] = 1
		return v
	}
	norm = math.Sqrt(norm)
	for i := range v {
		v[i] /= norm
	}
	return v
}

func main() {
	if dim < 2 || dim > 4096 {
		log.Fatalf("CONFIG_DIM must be between 2 and 4096")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /v1/model-info", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"name": modelName(), "dim": dim})
	})
	mux.HandleFunc("POST /v1/embed", func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		var req struct {
			Text string `json:"text"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid_request", "text is required")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"vector": embed(req.Text), "dim": dim, "model": modelName()})
	})
	serve(mux)
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
