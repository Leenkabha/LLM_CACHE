package safehttp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestValidateURLRejectsUnsafeByDefault(t *testing.T) {
	bad := map[string]string{
		"http scheme":        "http://plugin.example.com",
		"ftp":                "ftp://plugin.example.com",
		"userinfo":           "https://user:pw@plugin.example.com",
		"query":              "https://plugin.example.com/?token=abc",
		"empty query marker": "https://plugin.example.com/?",
		"fragment":           "https://plugin.example.com/#x",
		"loopback":           "https://127.0.0.1",
		"loopback v6":        "https://[::1]",
		"localhost":          "https://localhost",
		"private 10":         "https://10.1.2.3",
		"private 192":        "https://192.168.0.5",
		"private 172":        "https://172.16.5.5",
		"cgnat":              "https://100.64.1.1",
		"link local":         "https://169.254.1.1",
		"metadata ip":        "https://169.254.169.254",
		"metadata name":      "https://metadata.google.internal",
		"ula v6":             "https://[fd00::1]",
		"mapped loopback":    "https://[::ffff:127.0.0.1]",
		"single label":       "https://vectorstore",
		".internal":          "https://db.internal",
		"unspecified":        "https://0.0.0.0",
		"not a url":          "plugin.example.com",
		"empty":              "",
	}
	for name, raw := range bad {
		if _, err := ValidateURL(raw, Policy{}); err == nil {
			t.Errorf("%s: %q accepted", name, raw)
		}
	}
	if _, err := ValidateURL("https://plugin.example.com/base/", Policy{}); err != nil {
		t.Errorf("valid URL rejected: %v", err)
	}
	if _, err := ValidateURL("https://1.1.1.1", Policy{}); err != nil {
		t.Errorf("public IP rejected: %v", err)
	}
}

func TestDevelopmentModeAllowsLocalButNeverMetadata(t *testing.T) {
	dev := Policy{AllowInsecure: true}
	for _, ok := range []string{"http://127.0.0.1:8080", "http://localhost:9000", "http://example-llm:8090", "http://10.0.0.5", "https://[::1]:1"} {
		if _, err := ValidateURL(ok, dev); err != nil {
			t.Errorf("%s rejected in dev mode: %v", ok, err)
		}
	}
	for _, bad := range []string{"http://169.254.169.254", "http://metadata.google.internal", "http://[fd00:ec2::254]", "http://100.100.100.200", "http://u:p@localhost", "http://localhost/?k=v"} {
		if _, err := ValidateURL(bad, dev); err == nil {
			t.Errorf("%s accepted in dev mode", bad)
		}
	}
}

func TestClientBlocksLoopbackAtDialTime(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("hi")) }))
	defer srv.Close()

	strict := NewClient(Policy{}, 2*time.Second)
	if _, err := strict.Get(srv.URL); err == nil {
		t.Fatal("strict client reached a loopback server")
	}
	dev := NewClient(Policy{AllowInsecure: true}, 2*time.Second)
	resp, err := dev.Get(srv.URL)
	if err != nil {
		t.Fatalf("dev client: %v", err)
	}
	resp.Body.Close()
}

func TestClientDoesNotFollowRedirects(t *testing.T) {
	hits := 0
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits++ }))
	defer target.Close()
	redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer redir.Close()
	resp, err := NewClient(Policy{AllowInsecure: true}, 2*time.Second).Get(redir.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound || hits != 0 {
		t.Fatalf("status=%d target hits=%d; redirect was followed", resp.StatusCode, hits)
	}
}

func TestClientBlocksMetadataEvenInDevMode(t *testing.T) {
	c := NewClient(Policy{AllowInsecure: true}, time.Second)
	if _, err := c.Get("http://169.254.169.254/latest/meta-data"); err == nil {
		t.Fatal("dev client reached the metadata address")
	}
}

func TestReadLimited(t *testing.T) {
	if _, err := ReadLimited(strings.NewReader(strings.Repeat("a", 11)), 10); err != ErrTooLarge {
		t.Fatalf("err = %v", err)
	}
	if b, err := ReadLimited(strings.NewReader("abcde"), 5); err != nil || string(b) != "abcde" {
		t.Fatalf("got %q, %v", b, err)
	}
}

func TestIsContextErr(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if !IsContextErr(ctx, nil) {
		t.Fatal("canceled context not detected")
	}
}
