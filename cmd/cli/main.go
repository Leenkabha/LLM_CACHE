// Command cli is the v1 user-facing client for the semantic cache.
//
// Usage:
//
//	cli query "your prompt here"
//	cli stats
//	cli flush
//	cli policy lru|lfu
//
// The orchestrator URL is read from ORCH_URL (default http://localhost:8080).
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

func orchURL() string {
	if v := os.Getenv("ORCH_URL"); v != "" {
		return v
	}
	return "http://localhost:8080"
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "query":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: cli query \"prompt\"")
			os.Exit(1)
		}
		query(strings.Join(os.Args[2:], " "))
	case "stats":
		get("/stats")
	case "flush":
		post("/flush", nil)
	case "policy":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: cli policy lru|lfu")
			os.Exit(1)
		}
		post("/policy", map[string]string{"policy": os.Args[2]})
	default:
		usage()
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  cli query "prompt"   submit a prompt
  cli stats            show cache statistics
  cli flush            clear the cache
  cli policy lru|lfu   switch eviction policy`)
}

func query(prompt string) {
	var out struct {
		Reply     string  `json:"reply"`
		CacheHit  bool    `json:"cache_hit"`
		Distance  float64 `json:"distance"`
		LatencyMS int64   `json:"latency_ms"`
		Source    string  `json:"source"`
	}
	if err := request(http.MethodPost, "/query", map[string]string{"prompt": prompt}, &out); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	fmt.Printf("%s\n\n", out.Reply)
	fmt.Printf("[source=%s cache_hit=%v distance=%.4f latency=%dms]\n",
		out.Source, out.CacheHit, out.Distance, out.LatencyMS)
}

func get(path string) {
	var out map[string]any
	if err := request(http.MethodGet, path, nil, &out); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	printJSON(out)
}

func post(path string, body any) {
	var out map[string]any
	if err := request(http.MethodPost, path, body, &out); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	printJSON(out)
}

func request(method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, orchURL()+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("orchestrator unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func printJSON(v any) {
	b, _ := json.MarshalIndent(v, "", "  ")
	fmt.Println(string(b))
}
