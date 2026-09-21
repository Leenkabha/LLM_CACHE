// Command plugin-controller is the internal service that builds, pulls, scans,
// starts and proxies to plugin containers on behalf of the orchestrator. It is
// the only process that needs container-runtime access, and it must never be
// exposed publicly. See docs/PLUGIN_SECURITY.md.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/leenkabha/llm_cache/internal/plugins/controller"
	"github.com/leenkabha/llm_cache/internal/plugins/manifest"
)

func getenv(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func boolEnv(k string) bool {
	b, _ := strconv.ParseBool(os.Getenv(k))
	return b
}

func durEnv(k string) time.Duration {
	d, err := time.ParseDuration(os.Getenv(k))
	if err != nil {
		if os.Getenv(k) != "" {
			log.Fatalf("%s must be a duration such as 10m", k)
		}
		return 0
	}
	return d
}

func bytesEnv(k string) int64 {
	v := os.Getenv(k)
	if v == "" {
		return 0
	}
	n, err := manifest.ParseBytes(v)
	if err != nil {
		log.Fatalf("%s: %v", k, err)
	}
	return n
}

func main() {
	cfg := controller.Config{
		Token:            os.Getenv("PLUGIN_CONTROLLER_TOKEN"),
		RunnerDir:        getenv("PLUGIN_RUNNER_DIR", ""),
		BuildTimeout:     durEnv("PLUGIN_BUILD_TIMEOUT"),
		HealthWait:       durEnv("PLUGIN_HEALTH_TIMEOUT"),
		MaxImageBytes:    bytesEnv("PLUGIN_MAX_IMAGE_BYTES"),
		MaxSourceBytes:   bytesEnv("PLUGIN_MAX_SOURCE_BYTES"),
		MemoryLimit:      bytesEnv("PLUGIN_MEMORY_LIMIT"),
		AllowLocalImages: boolEnv("PLUGIN_ALLOW_LOCAL_IMAGES"),
		AllowLocalGit:    boolEnv("PLUGIN_ALLOW_LOCAL_GIT"),
		AllowEgress:      boolEnv("PLUGIN_ALLOW_EGRESS"),
		RequireScan:      boolEnv("PLUGIN_REQUIRE_SCAN"),
		Publish:          boolEnv("PLUGIN_CONTROLLER_PUBLISH"),
		SelfContainer:    os.Getenv("PLUGIN_CONTROLLER_CONTAINER"),
	}
	if v := os.Getenv("PLUGIN_CPU_LIMIT"); v != "" {
		c, err := manifest.ParseCPU(v)
		if err != nil {
			log.Fatalf("PLUGIN_CPU_LIMIT: %v", err)
		}
		cfg.CPULimit = c
	}

	var scanner controller.Scanner = controller.NoScanner{}
	switch strings.ToLower(getenv("PLUGIN_SCANNER", "none")) {
	case "none", "":
	case "trivy":
		scanner = controller.Trivy{FailOn: strings.Split(getenv("PLUGIN_SCAN_FAIL_ON", "CRITICAL,HIGH"), ",")}
	default:
		log.Fatalf("PLUGIN_SCANNER must be none or trivy")
	}

	// Inside a container (and not in publish mode) the controller joins each
	// plugin's private network, identifying itself by its own container ID.
	if !cfg.Publish && cfg.SelfContainer == "" {
		if _, err := os.Stat("/.dockerenv"); err == nil {
			cfg.SelfContainer, _ = os.Hostname()
		}
	}

	rt := controller.NewDockerCLI()
	if !cfg.Publish {
		rt.Self = cfg.SelfContainer
	}
	pctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	if err := rt.Ping(pctx); err != nil {
		log.Fatalf("cannot reach the container runtime: %v", err)
	}
	cancel()

	srv, err := controller.New(cfg, rt, scanner)
	if err != nil {
		log.Fatalf("plugin-controller: %v", err)
	}
	defer srv.Close()

	addr := getenv("PLUGIN_CONTROLLER_ADDR", "127.0.0.1:8090")
	hs := &http.Server{Addr: addr, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		log.Printf("plugin-controller listening on %s (scanner=%s publish=%v)", addr, scanner.Name(), cfg.Publish)
		if err := hs.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	sctx, scancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer scancel()
	_ = hs.Shutdown(sctx)
}
