package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestExampleServer(t *testing.T) {
	for _, tc := range []struct {
		body   string
		status int
	}{
		{`{"model":"demo","prompt":"hello"}`, 200}, {`{"prompt":"hello"}`, 400}, {`bad`, 400},
	} {
		rec := httptest.NewRecorder()
		routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/complete", strings.NewReader(tc.body)))
		if rec.Code != tc.status {
			t.Fatalf("status=%d want=%d", rec.Code, tc.status)
		}
		if rec.Code == 200 {
			var out map[string]string
			if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out["reply"] != "[example demo] hello" {
				t.Fatalf("response=%s", rec.Body.String())
			}
		}
	}
}
