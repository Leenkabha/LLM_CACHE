package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestQueryDisplaysResults(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       []string
	}{
		{"multiple", `{"reply":"first","cache_hit":true,"source":"cache","distance":0.02,"results":[{"id":"a","reply":"first","distance":0.02},{"id":"b","reply":"second","distance":0.1}]}`, []string{"1. first", "id=a distance=0.0200", "2. second", "id=b distance=0.1000"}},
		{"single", `{"reply":"first","cache_hit":true,"source":"cache","distance":0.02,"results":[{"id":"a","reply":"first","distance":0.02}]}`, []string{"first\n\n", "source=cache"}},
		{"miss", `{"reply":"fresh","cache_hit":false,"source":"llm","distance":-1,"results":[]}`, []string{"fresh\n\n", "source=llm"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			t.Setenv("ORCH_URL", server.URL)
			reader, writer, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			original := os.Stdout
			os.Stdout = writer
			defer func() { os.Stdout = original; writer.Close() }()
			query("test")
			writer.Close()
			os.Stdout = original
			data, err := io.ReadAll(reader)
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range tc.want {
				if !strings.Contains(string(data), want) {
					t.Fatalf("output %q missing %q", data, want)
				}
			}
			if tc.name == "multiple" && strings.Index(string(data), "1. first") > strings.Index(string(data), "2. second") {
				t.Fatal("wrong order")
			}
		})
	}
}
