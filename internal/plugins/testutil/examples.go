// Package testutil builds and runs the SDK example plugins as real processes so
// tests exercise exactly what a developer would copy.
package testutil

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

// RepoRoot returns the repository root (two levels above internal/plugins/testutil).
func RepoRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "..")
}

var (
	buildMu  sync.Mutex
	built    = map[string]string{}
	buildDir string
)

func goBin() string {
	if p, err := exec.LookPath("go"); err == nil {
		return p
	}
	return filepath.Join(runtime.GOROOT(), "bin", "go")
}

// BuildExample compiles sdk/examples/<name> once per test binary.
func BuildExample(t testing.TB, name string) string {
	t.Helper()
	buildMu.Lock()
	defer buildMu.Unlock()
	if p, ok := built[name]; ok {
		return p
	}
	if buildDir == "" {
		d, err := os.MkdirTemp("", "llmcache-sdk-*")
		if err != nil {
			t.Fatal(err)
		}
		buildDir = d
	}
	out := filepath.Join(buildDir, name)
	cmd := exec.Command(goBin(), "build", "-buildvcs=false", "-o", out, ".")
	cmd.Dir = filepath.Join(RepoRoot(), "sdk", "examples", name)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOFLAGS=-mod=mod")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building sdk example %s: %v\n%s", name, err, b)
	}
	built[name] = out
	return out
}

// FreePort returns an unused loopback TCP port.
func FreePort(t testing.TB) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// Example is a running SDK example.
type Example struct {
	URL  string
	Port int
	cmd  *exec.Cmd
}

// Stop terminates the process.
func (e *Example) Stop() {
	if e.cmd != nil && e.cmd.Process != nil {
		_ = e.cmd.Process.Kill()
		_, _ = e.cmd.Process.Wait()
	}
}

// StartExample builds and starts an example with extra environment variables
// and waits for /health.
func StartExample(t testing.TB, name string, env map[string]string) *Example {
	t.Helper()
	bin := BuildExample(t, name)
	port := FreePort(t)
	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(), fmt.Sprintf("PORT=%d", port))
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	ex := &Example{URL: fmt.Sprintf("http://127.0.0.1:%d", port), Port: port, cmd: cmd}
	t.Cleanup(ex.Stop)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ex.URL+"/health", nil)
		resp, err := http.DefaultClient.Do(req)
		cancel()
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return ex
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("example %s did not become healthy", name)
	return nil
}
