package orchestrator

import (
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func pluginsPage(t *testing.T) (*httptest.ResponseRecorder, string) {
	t.Helper()
	s, _, _ := queryFixture(t, &fixedSearch{}, 1)
	s.SetAdminHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/plugins", nil))
	return rec, rec.Body.String()
}

func TestPluginsPageIsOnlyServedWhenThePluginAPIIsMounted(t *testing.T) {
	s, _, _ := queryFixture(t, &fixedSearch{}, 1)
	for _, path := range []string{"/plugins", "/admin/plugins"} {
		rec := httptest.NewRecorder()
		s.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s without the platform = %d, want 404 (existing behaviour unchanged)", path, rec.Code)
		}
	}
	rec, body := pluginsPage(t)
	if rec.Code != 200 || !strings.Contains(body, "<title>Plugins - LLM Semantic Cache</title>") {
		t.Fatalf("status=%d", rec.Code)
	}
}

func TestPluginsPageSecurityHeadersAndScriptHash(t *testing.T) {
	rec, body := pluginsPage(t)
	csp := rec.Header().Get("Content-Security-Policy")
	m := regexp.MustCompile(`(?s)<script>(.*)</script>`).FindStringSubmatch(body)
	if m == nil {
		t.Fatal("no inline script")
	}
	sum := sha256.Sum256([]byte(m[1]))
	if want := "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"; !strings.Contains(csp, "script-src "+want) {
		t.Fatalf("CSP %q does not allow exactly the page script (%s)", csp, want)
	}
	for _, bad := range []string{"script-src 'unsafe-inline'", "'unsafe-eval'", "default-src *", "frame-ancestors *"} {
		if strings.Contains(csp, bad) {
			t.Errorf("CSP contains %q: %s", bad, csp)
		}
	}
	for _, want := range []string{"default-src 'none'", "connect-src 'self'", "frame-ancestors 'none'", "base-uri 'none'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP lacks %q", want)
		}
	}
	if rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("X-Content-Type-Options") != "nosniff" || rec.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Errorf("headers = %v", rec.Header())
	}
}

// The page must never insert API data as HTML, persist the token or a secret, or
// put credentials in a URL.
func TestPluginsPageJSFollowsTheSecurityRules(t *testing.T) {
	_, body := pluginsPage(t)
	js := regexp.MustCompile(`(?s)<script>(.*)</script>`).FindStringSubmatch(body)[1]
	for _, forbidden := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "eval(", "new Function", "localStorage", "sessionStorage", "indexedDB", "document.cookie", "location.hash", "location.search", "?token=", "&token="} {
		if strings.Contains(js, forbidden) {
			t.Errorf("the page script uses %q", forbidden)
		}
	}
	// Secrets are write-only: password inputs, cleared after use, never filled from the API.
	if !strings.Contains(js, "autocomplete: 'new-password'") || !strings.Contains(js, "el.value = ''") {
		t.Error("secret inputs must be non-autofilling and cleared after submit")
	}
	if strings.Contains(js, ".value = s.") || strings.Contains(js, "value: s.value") {
		t.Error("a stored secret value is rendered into an input")
	}
	// Auth goes in a header, credentials are omitted, responses are not cached.
	for _, want := range []string{"'Authorization': 'Bearer ' + token", "credentials: 'omit'", "cache: 'no-store'"} {
		if !strings.Contains(js, want) {
			t.Errorf("missing %q", want)
		}
	}
	// Accessibility basics.
	for _, want := range []string{`aria-live="polite"`, `role="alert"`, `<label for="f-endpoint"`, `aria-label="Close"`, `lang="en"`, `name="viewport"`} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	// Every state the backend reports has a label.
	for _, st := range []string{"Draft", "Validating manifest", "Testing contract", "Building", "Scanning", "Starting candidate", "Migrating or rebuilding", "Checking health", "Activating", "Active", "Failed", "Rolling back", "Rolled back"} {
		if !strings.Contains(js, "'"+st+"'") {
			t.Errorf("state label %q missing", st)
		}
	}
}

func TestQueryUIStillWorksAndLinksToPluginsOnlyWhenAvailable(t *testing.T) {
	s, _, _ := queryFixture(t, &fixedSearch{}, 1)
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	body := rec.Body.String()
	for _, want := range []string{"fetch('/query'", `id="prompt"`, `id="plugins-link" hidden`} {
		if !strings.Contains(body, want) {
			t.Errorf("query UI lacks %q", want)
		}
	}
}
