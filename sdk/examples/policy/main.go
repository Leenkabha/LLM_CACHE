// A minimal eviction-policy plugin (LRU) for the LLM Cache plugin platform.
//
// Contract (v1):
//
//	GET  /v1/policy -> {"name":"sdk-lru"}
//	POST /v1/events {"events":[{"op":"hit|insert|delete","id":"a"}]} -> 200   (applied in order)
//	GET  /v1/victim -> {"id":"a","ok":true} | {"ok":false}                     (selects, never deletes)
//	POST /v1/flush  -> 200                                                     (policy stays usable)
//	GET  /health
//
// Unknown ids in hit/delete are ignored, repeating an insert never duplicates
// state, and Victim does not remove anything -- the platform deletes the entry
// and then sends a "delete" event.
package main

import (
	"container/list"
	"context"
	"crypto/subtle"
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

type lru struct {
	mu    sync.Mutex
	order *list.List // front = most recently used
	items map[string]*list.Element
}

func (p *lru) apply(op, id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	el, ok := p.items[id]
	switch op {
	case "hit":
		if ok {
			p.order.MoveToFront(el)
		}
	case "insert":
		if ok {
			p.order.MoveToFront(el)
		} else {
			p.items[id] = p.order.PushFront(id)
		}
	case "delete":
		if ok {
			p.order.Remove(el)
			delete(p.items, id)
		}
	}
}

func (p *lru) victim() (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if el := p.order.Back(); el != nil {
		return el.Value.(string), true
	}
	return "", false
}

func (p *lru) flush() {
	p.mu.Lock()
	p.order.Init()
	p.items = map[string]*list.Element{}
	p.mu.Unlock()
}

func main() {
	p := &lru{order: list.New(), items: map[string]*list.Element{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /v1/policy", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"name": "sdk-lru"})
	})
	mux.HandleFunc("POST /v1/events", func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 4<<20)
		var req struct {
			Events []struct {
				Op string `json:"op"`
				ID string `json:"id"`
			} `json:"events"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid_request", "events required")
			return
		}
		for _, e := range req.Events {
			if e.ID == "" || (e.Op != "hit" && e.Op != "insert" && e.Op != "delete") {
				writeErr(w, http.StatusBadRequest, "invalid_request", "bad event")
				return
			}
		}
		for _, e := range req.Events {
			p.apply(e.Op, e.ID)
		}
		writeJSON(w, http.StatusOK, map[string]int{"applied": len(req.Events)})
	})
	mux.HandleFunc("GET /v1/victim", func(w http.ResponseWriter, _ *http.Request) {
		if id, ok := p.victim(); ok {
			writeJSON(w, http.StatusOK, map[string]any{"id": id, "ok": true})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": false})
	})
	mux.HandleFunc("POST /v1/flush", func(w http.ResponseWriter, _ *http.Request) {
		p.flush()
		writeJSON(w, http.StatusOK, map[string]string{"status": "flushed"})
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
