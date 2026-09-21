// Package admin exposes the plugin manager as an authenticated JSON API under
// /admin/. Every route requires a configured, non-empty ADMIN_TOKEN; when the
// token is unset, or plugin installation is disabled, the API answers 503 and
// reveals nothing.
//
// Bodies are strict JSON (unknown fields and trailing data are errors) and
// size-limited. Responses are built from manager.View, which contains no secret
// material (no plaintext, no ciphertext), and every error message is scrubbed.
package admin

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/leenkabha/llm_cache/internal/plugins/manager"
	"github.com/leenkabha/llm_cache/internal/plugins/registry"
	"github.com/leenkabha/llm_cache/internal/plugins/secrets"
)

// MaxBodyBytes bounds a request body.
const MaxBodyBytes = 256 << 10

// API is the HTTP handler.
type API struct {
	m          *manager.Manager
	token      string
	limiter    *failLimiter
	mux        *http.ServeMux
	trustProxy bool
}

// Option tunes the API.
type Option func(*API)

// WithAuthFailureLimit sets how many failed authentications one address may make
// per minute before it is refused (default 10).
func WithAuthFailureLimit(n int) Option { return func(a *API) { a.limiter.max = n } }

// WithTrustProxy takes the client address from X-Forwarded-For (first entry), as
// the orchestrator's other admin protections do with TRUST_PROXY. Enable it only
// when the orchestrator is reachable solely through a reverse proxy; otherwise
// every visitor would share the proxy's address and one attacker could lock the
// administrator out of signing in.
func WithTrustProxy(v bool) Option { return func(a *API) { a.trustProxy = v } }

// New builds the handler. An empty token disables the API.
func New(m *manager.Manager, adminToken string, opts ...Option) *API {
	a := &API{m: m, token: adminToken, limiter: &failLimiter{max: 10, window: time.Minute, hits: map[string][]time.Time{}}}
	for _, o := range opts {
		o(a)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /admin/plugins", a.list)
	mux.HandleFunc("POST /admin/plugins/verify", a.verify)
	mux.HandleFunc("POST /admin/plugins/install", a.install)
	mux.HandleFunc("GET /admin/plugins/{id}", a.get)
	mux.HandleFunc("POST /admin/plugins/{id}/activate", a.activate)
	mux.HandleFunc("POST /admin/plugins/{id}/deactivate", a.deactivate)
	mux.HandleFunc("POST /admin/plugins/{id}/upgrade", a.upgrade)
	mux.HandleFunc("POST /admin/plugins/{id}/rollback", a.rollback)
	mux.HandleFunc("DELETE /admin/plugins/{id}", a.remove)
	mux.HandleFunc("GET /admin/plugins/{id}/logs", a.logs)
	mux.HandleFunc("GET /admin/audit", a.audit)
	a.mux = mux
	return a
}

func (a *API) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	if a.token == "" || !a.m.Enabled() {
		writeErr(w, http.StatusServiceUnavailable, "plugin management is disabled (it needs ADMIN_TOKEN, PLUGIN_SECRET_KEY and ENABLE_PLUGIN_INSTALLATION=true)")
		return
	}
	ip := a.clientIP(r)
	if !a.limiter.allowed(ip) {
		h.Set("Retry-After", "60")
		writeErr(w, http.StatusTooManyRequests, "too many failed authentication attempts")
		return
	}
	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || subtle.ConstantTimeCompare([]byte(got), []byte(a.token)) != 1 {
		a.limiter.fail(ip)
		h.Set("WWW-Authenticate", "Bearer")
		writeErr(w, http.StatusUnauthorized, "admin token required")
		return
	}
	a.mux.ServeHTTP(w, r)
}

func (a *API) clientIP(r *http.Request) string {
	if a.trustProxy {
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

// failLimiter counts failed authentications per address.
type failLimiter struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	hits   map[string][]time.Time
}

func (l *failLimiter) prune(ip string, now time.Time) []time.Time {
	kept := l.hits[ip][:0]
	for _, t := range l.hits[ip] {
		if now.Sub(t) < l.window {
			kept = append(kept, t)
		}
	}
	if len(kept) == 0 {
		delete(l.hits, ip)
	} else {
		l.hits[ip] = kept
	}
	return kept
}

func (l *failLimiter) allowed(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.prune(ip, time.Now())) < l.max
}

func (l *failLimiter) fail(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.hits) > 10000 {
		l.hits = map[string][]time.Time{}
	}
	l.hits[ip] = append(l.hits[ip], time.Now())
}

// ---- helpers -----------------------------------------------------------------

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"error": secrets.Scrub(msg)})
}

// decode reads one strict JSON object. An empty body is allowed when optional.
func decode(w http.ResponseWriter, r *http.Request, dst any, optional bool) bool {
	r.Body = http.MaxBytesReader(w, r.Body, MaxBodyBytes)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeErr(w, http.StatusRequestEntityTooLarge, "request body too large")
		} else {
			writeErr(w, http.StatusBadRequest, "could not read request body")
		}
		return false
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		if optional {
			return true
		}
		writeErr(w, http.StatusBadRequest, "a JSON body is required")
		return false
	}
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON: "+shortErr(err))
		return false
	}
	if dec.More() {
		writeErr(w, http.StatusBadRequest, "invalid JSON: trailing data after the object")
		return false
	}
	return true
}

func shortErr(err error) string {
	s := err.Error()
	if len(s) > 160 {
		s = s[:160]
	}
	return s
}

// fail maps a manager error to an HTTP response.
func fail(w http.ResponseWriter, err error) {
	var (
		req *manager.RequirementError
		inc *manager.IncompatibleError
		inv *manager.InvalidError
	)
	switch {
	case errors.As(err, &req):
		writeJSON(w, http.StatusConflict, map[string]any{"error": secrets.Scrub(req.Message), "requires": req})
	case errors.As(err, &inc):
		writeErr(w, http.StatusUnprocessableEntity, inc.Reason)
	case errors.As(err, &inv):
		writeErr(w, http.StatusBadRequest, inv.Msg)
	case errors.Is(err, manager.ErrBusy):
		writeErr(w, http.StatusConflict, err.Error())
	case errors.Is(err, manager.ErrDisabled):
		writeErr(w, http.StatusServiceUnavailable, err.Error())
	case manager.IsNotFound(err):
		writeErr(w, http.StatusNotFound, "plugin not found")
	default:
		writeErr(w, http.StatusInternalServerError, err.Error())
	}
}

type confirmBody struct {
	Confirmations registry.Confirmations `json:"confirmations"`
}

// ---- handlers ----------------------------------------------------------------

func (a *API) list(w http.ResponseWriter, r *http.Request) {
	recs, err := a.m.List()
	if err != nil {
		fail(w, err)
		return
	}
	views := make([]manager.View, 0, len(recs))
	for _, rec := range recs {
		views = append(views, manager.ViewOf(rec))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"plugins": views, "components": a.m.Components(), "active": a.m.ActiveIDs(), "settings": a.m.Settings(),
	})
}

func (a *API) get(w http.ResponseWriter, r *http.Request) {
	rec, err := a.m.Get(r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"plugin": manager.ViewOf(rec)})
}

func (a *API) verify(w http.ResponseWriter, r *http.Request) {
	var req manager.InstallRequest
	if !decode(w, r, &req, false) {
		return
	}
	res, err := a.m.Verify(r.Context(), req)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (a *API) install(w http.ResponseWriter, r *http.Request) {
	var req manager.InstallRequest
	if !decode(w, r, &req, false) {
		return
	}
	rec, err := a.m.Install(r.Context(), req)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"plugin": manager.ViewOf(rec)})
}

func (a *API) upgrade(w http.ResponseWriter, r *http.Request) {
	var req manager.InstallRequest
	if !decode(w, r, &req, false) {
		return
	}
	rec, err := a.m.Upgrade(r.Context(), r.PathValue("id"), req)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"plugin": manager.ViewOf(rec)})
}

func (a *API) switchOp(op func(r *http.Request, id string, c registry.Confirmations) (*manager.Result, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body confirmBody
		if !decode(w, r, &body, true) {
			return
		}
		res, err := op(r, r.PathValue("id"), body.Confirmations)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"plugin": manager.ViewOf(res.Record), "warnings": res.Warnings})
	}
}

func (a *API) activate(w http.ResponseWriter, r *http.Request) {
	a.switchOp(func(r *http.Request, id string, c registry.Confirmations) (*manager.Result, error) {
		return a.m.Activate(r.Context(), id, c)
	})(w, r)
}

func (a *API) deactivate(w http.ResponseWriter, r *http.Request) {
	a.switchOp(func(r *http.Request, id string, c registry.Confirmations) (*manager.Result, error) {
		return a.m.Deactivate(r.Context(), id, c)
	})(w, r)
}

func (a *API) rollback(w http.ResponseWriter, r *http.Request) {
	a.switchOp(func(r *http.Request, id string, c registry.Confirmations) (*manager.Result, error) {
		return a.m.Rollback(r.Context(), id, c)
	})(w, r)
}

func (a *API) remove(w http.ResponseWriter, r *http.Request) {
	if err := a.m.Delete(r.Context(), r.PathValue("id")); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (a *API) logs(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit < 1 || limit > 500 {
		limit = 200
	}
	lines, err := a.m.Logs(r.PathValue("id"), limit)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"lines": lines})
}

func (a *API) audit(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit < 1 || limit > 500 {
		limit = 100
	}
	events, err := a.m.Audit(r.URL.Query().Get("plugin"), limit)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events})
}
