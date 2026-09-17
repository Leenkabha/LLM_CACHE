package orchestrator

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestUIServedAtRoot(t *testing.T) {
	s, _, _ := queryFixture(t, &fixedSearch{}, 1)

	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("Content-Type=%q, want text/html", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "<title>LLM Semantic Cache</title>") {
		t.Fatalf("body missing expected title: %s", body)
	}
	if !strings.Contains(body, "fetch('/query'") {
		t.Fatalf("body missing expected fetch call to /query")
	}
}

// The root route must not shadow the existing API routes -- it only matches
// the exact root path ("/{$}"), not a subtree.
func TestUIDoesNotShadowAPIRoutes(t *testing.T) {
	s, _, _ := queryFixture(t, &fixedSearch{}, 1)

	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/stats", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/stats status=%d, want 200", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "<!doctype html>") {
		t.Fatal("/stats returned the UI instead of the stats API")
	}
}

func TestUIRejectsNonGET(t *testing.T) {
	s, _, _ := queryFixture(t, &fixedSearch{}, 1)

	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", nil))
	if rec.Code == http.StatusOK {
		t.Fatalf("POST / status=%d, want non-200 (method not allowed)", rec.Code)
	}
}
