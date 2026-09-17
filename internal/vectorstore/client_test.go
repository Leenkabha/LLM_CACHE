package vectorstore

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSearchHTTP(t *testing.T) {
	for _, tc := range []struct {
		name, body   string
		status, want int
		invalid      bool
	}{
		{"ordered matches", `{"hit":true,"matches":[{"id":"a","distance":0.02},{"id":"b","distance":0.1}]}`, 200, 2, false},
		{"miss", `{"hit":false,"matches":[]}`, 200, 0, false},
		{"legacy service", `{"hit":true,"id":"a","distance":0.02}`, 200, 0, true},
		{"bad json", `{`, 200, 0, true},
		{"service error", `{}`, 500, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "POST" || r.URL.Path != "/search" {
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				}
				var req searchRequest
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
				}
				if req.TopK != 3 || req.Threshold != 0.1 || len(req.Vector) != 2 {
					t.Errorf("request=%+v", req)
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			matches, err := NewHTTP(server.URL).Search(context.Background(), []float64{1, 0}, 3, 0.1)
			if (err != nil) != tc.invalid {
				t.Fatalf("error=%v", err)
			}
			if len(matches) != tc.want {
				t.Fatalf("matches=%+v", matches)
			}
			if tc.want == 2 && (matches[0].ID != "a" || matches[1].ID != "b" || matches[1].Distance != 0.1) {
				t.Fatalf("order/values=%+v", matches)
			}
		})
	}
}

func TestSearchRejectsInvalidK(t *testing.T) {
	for _, k := range []int{0, -1} {
		if _, err := NewHTTP("http://unused.invalid").Search(context.Background(), []float64{1}, k, 0.1); err == nil {
			t.Fatalf("accepted k=%d", k)
		}
	}
}

// Regression test: Size() must check the HTTP status code, the same bug
// class previously fixed in Delete(). A non-200 response with a body that
// happens to decode cleanly into sizeResponse (e.g. an empty JSON object)
// must not be reported as a successful size of 0.
func TestSizeHTTP(t *testing.T) {
	for _, tc := range []struct {
		name, body   string
		status, want int
		invalid      bool
	}{
		{"ok", `{"size":5}`, 200, 5, false},
		{"error status with json-ish body", `{}`, 500, 0, true},
		{"error status with error body", `{"detail":"not found"}`, 404, 0, true},
		{"bad json", `{`, 200, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" || r.URL.Path != "/size" {
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			size, err := NewHTTP(server.URL).Size(context.Background())
			if (err != nil) != tc.invalid {
				t.Fatalf("error=%v, want invalid=%v", err, tc.invalid)
			}
			if !tc.invalid && size != tc.want {
				t.Fatalf("size=%d, want %d", size, tc.want)
			}
		})
	}
}

func TestSizeHTTPUnreachable(t *testing.T) {
	if _, err := NewHTTP("http://127.0.0.1:1").Size(context.Background()); err == nil {
		t.Fatal("expected error for unreachable vector store")
	}
}
