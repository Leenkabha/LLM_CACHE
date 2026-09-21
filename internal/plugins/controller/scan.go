package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/leenkabha/llm_cache/internal/plugins/registry"
)

// Scanner inspects an image for known vulnerabilities.
type Scanner interface {
	Scan(ctx context.Context, imageRef string) (*registry.ScanReport, error)
	Name() string
}

// NoScanner records honestly that no scan happened.
type NoScanner struct{}

func (NoScanner) Name() string { return "none" }
func (NoScanner) Scan(context.Context, string) (*registry.ScanReport, error) {
	return &registry.ScanReport{Scanner: "none", Status: "skipped", Message: "no vulnerability scanner is configured (set PLUGIN_SCANNER=trivy)", ScannedAt: time.Now().UTC()}, nil
}

// Trivy runs the trivy CLI and fails the scan on findings at or above the
// configured severities.
type Trivy struct {
	Bin    string
	FailOn []string // e.g. CRITICAL, HIGH
	Env    []string
}

func (Trivy) Name() string { return "trivy" }

func (t Trivy) Scan(ctx context.Context, ref string) (*registry.ScanReport, error) {
	if !imageRefRE.MatchString(ref) {
		return nil, errors.New("invalid image reference")
	}
	bin := t.Bin
	if bin == "" {
		bin = "trivy"
	}
	cmd := exec.CommandContext(ctx, bin, "image", "--quiet", "--format", "json", "--severity", "UNKNOWN,LOW,MEDIUM,HIGH,CRITICAL", ref)
	cmd.Env = append([]string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.TempDir()}, t.Env...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("scanner failed: %w: %s", err, tail(errb.String(), 300))
	}
	var res struct {
		Results []struct {
			Vulnerabilities []struct{ Severity string } `json:"Vulnerabilities"`
		} `json:"Results"`
	}
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		return nil, errors.New("scanner output was not understood")
	}
	counts := map[string]int{}
	for _, r := range res.Results {
		for _, v := range r.Vulnerabilities {
			counts[strings.ToUpper(v.Severity)]++
		}
	}
	rep := &registry.ScanReport{Scanner: "trivy", Status: "passed", Counts: counts, ScannedAt: time.Now().UTC()}
	for _, sev := range t.FailOn {
		if n := counts[strings.ToUpper(sev)]; n > 0 {
			rep.Status = "failed"
			rep.Message = fmt.Sprintf("%d %s vulnerabilities found", n, strings.ToUpper(sev))
			break
		}
	}
	return rep, nil
}
