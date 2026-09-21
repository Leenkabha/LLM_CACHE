package manager_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/leenkabha/llm_cache/internal/plugins"
	"github.com/leenkabha/llm_cache/internal/plugins/manager"
	"github.com/leenkabha/llm_cache/internal/plugins/registry"
	"github.com/leenkabha/llm_cache/internal/plugins/testutil"
)

func sdkManifest(t *testing.T, example string) string {
	b, err := os.ReadFile(filepath.Join(testutil.RepoRoot(), "sdk", "examples", example, "plugin.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func newContainerHarness(t *testing.T, window time.Duration) (*harness, *testutil.FakeController) {
	fc := testutil.NewFakeController(t)
	h := newHarness(t, opts{ctl: fc, cfg: func(c *manager.Config) {
		c.ControllerURL, c.ControllerToken = "http://controller.invalid", "ctl-token"
		if window > 0 {
			c.RollbackWindow = window
		}
	}})
	return h, fc
}

func TestImageInstallPinsDigestInjectsSecretsAtRuntimeAndActivates(t *testing.T) {
	h, fc := newContainerHarness(t, 0)
	fc.Register("ghcr.io/acme/echo-llm:1.0", "llm")
	const secret = "image-mode-secret-value"

	rec, err := h.m.Install(context.Background(), manager.InstallRequest{
		Type: "llm", Mode: "image", Image: "ghcr.io/acme/echo-llm:1.0", Manifest: sdkManifest(t, "llm"),
		Config: map[string]any{"model": "demo"}, Secrets: map[string]string{"API_TOKEN": secret},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := h.waitState(rec.ID, plugins.StateVerified)
	if !strings.HasPrefix(got.ImageDigest, "sha256:") || len(got.ImageDigest) != 71 {
		t.Fatalf("digest = %q; the record must hold an immutable digest, not the mutable tag", got.ImageDigest)
	}
	if got.Scan == nil || got.Scan.Status != "skipped" || got.Provenance == nil {
		t.Fatalf("scan=%+v provenance=%+v", got.Scan, got.Provenance)
	}
	if got.Source.ImageRef != "ghcr.io/acme/echo-llm:1.0" || got.Mode != plugins.ModeImage {
		t.Fatalf("source = %+v", got.Source)
	}

	// The build/pull request carries no secrets and no configuration.
	for _, p := range fc.Prepared {
		b, _ := json.Marshal(p)
		if strings.Contains(string(b), secret) {
			t.Fatal("a secret was sent to the controller with the prepare (build) request")
		}
	}
	// The secret is injected only when the instance starts, from the encrypted store.
	found := false
	for _, s := range fc.Started {
		if s.Env["API_TOKEN"] == secret && s.Env["CONFIG_MODEL"] == "demo" && s.Digest == got.ImageDigest {
			found = true
		}
	}
	if !found {
		t.Fatalf("start requests = %+v", fc.Started)
	}
	stored, _ := h.reg.Get(rec.ID)
	if b, _ := json.Marshal(stored); strings.Contains(string(b), secret) {
		t.Fatal("secret stored in plaintext")
	}

	h.mustActivate(rec.ID, registry.Confirmations{})
	if r := h.query("through a container plugin"); !strings.Contains(r.Reply, "secret-configured") || !strings.Contains(r.Reply, "demo") {
		t.Fatalf("reply = %q", r.Reply)
	}
}

func TestRepositoryInstallReadsTheManifestFromTheRepoAndRecordsTheCommit(t *testing.T) {
	h, fc := newContainerHarness(t, 0)
	fc.Register("https://github.com/acme/queue-plugin", "queue")

	// Verify resolves the manifest (dry run) so a UI can render the form.
	vr, err := h.m.Verify(context.Background(), manager.InstallRequest{Type: "queue", Mode: "repository", Repo: "https://github.com/acme/queue-plugin", Revision: "main"})
	if err != nil || !vr.Valid || vr.Name != "sdk-memory-queue" || len(vr.Commit) != 40 {
		t.Fatalf("verify = %+v, %v", vr, err)
	}
	if !fc.Prepared[0].DryRun {
		t.Fatal("verify must not build")
	}
	if all, _ := h.m.List(); len(all) != 0 {
		t.Fatal("verify created a record")
	}

	rec, err := h.m.Install(context.Background(), manager.InstallRequest{Type: "queue", Mode: "repository", Repo: "https://github.com/acme/queue-plugin", Revision: "main"})
	if err != nil {
		t.Fatal(err)
	}
	got := h.waitState(rec.ID, plugins.StateVerified)
	if got.Name != "sdk-memory-queue" || got.Version != "1.0.0" || len(got.Commit) != 40 || got.Provenance.Commit != got.Commit {
		t.Fatalf("record = %+v", got)
	}
	if got.Source.RepoURL != "https://github.com/acme/queue-plugin" || got.Source.Revision != "main" {
		t.Fatalf("source = %+v", got.Source)
	}
	h.mustActivate(rec.ID, registry.Confirmations{})
	h.query("queue via container")
	h.waitCacheSize(1)
}

func TestPipelineFailuresLeaveNothingRunningAndAreScrubbed(t *testing.T) {
	ctx := context.Background()
	t.Run("prepare fails", func(t *testing.T) {
		h, fc := newContainerHarness(t, 0)
		fc.Register("ghcr.io/acme/x:1", "llm")
		fc.FailWith = errors.New("registry said: unauthorized; token=abc123secret456")
		rec, _ := h.m.Install(ctx, manager.InstallRequest{Type: "llm", Mode: "image", Image: "ghcr.io/acme/x:1", Manifest: sdkManifest(t, "llm")})
		h.m.WaitIdle()
		got, _ := h.m.Get(rec.ID)
		if got.State != plugins.StateFailed || strings.Contains(got.StateDetail, "abc123secret456") {
			t.Fatalf("state=%s detail=%q", got.State, got.StateDetail)
		}
		if len(fc.Started) != 0 {
			t.Fatal("an instance was started for a plugin that failed to prepare")
		}
	})
	t.Run("scan fails", func(t *testing.T) {
		h, fc := newContainerHarness(t, 0)
		fc.Register("ghcr.io/acme/x:1", "llm")
		fc.ScanState = "failed"
		rec, _ := h.m.Install(ctx, manager.InstallRequest{Type: "llm", Mode: "image", Image: "ghcr.io/acme/x:1", Manifest: sdkManifest(t, "llm")})
		h.m.WaitIdle()
		got, _ := h.m.Get(rec.ID)
		if got.State != plugins.StateFailed || !strings.Contains(got.StateDetail, "scan") {
			t.Fatalf("state=%s detail=%q", got.State, got.StateDetail)
		}
		if len(fc.Started) != 0 {
			t.Fatal("an unscanned-clean image was started")
		}
	})
	t.Run("manifest type mismatch from the repository", func(t *testing.T) {
		h, fc := newContainerHarness(t, 0)
		fc.Register("https://github.com/acme/policy", "policy")
		rec, _ := h.m.Install(ctx, manager.InstallRequest{Type: "queue", Mode: "repository", Repo: "https://github.com/acme/policy"})
		h.m.WaitIdle()
		got, _ := h.m.Get(rec.ID)
		if got.State != plugins.StateFailed || !strings.Contains(got.StateDetail, "type") {
			t.Fatalf("state=%s detail=%q", got.State, got.StateDetail)
		}
	})
	t.Run("required secret missing for a repository plugin", func(t *testing.T) {
		h, fc := newContainerHarness(t, 0)
		fc.Register("https://github.com/acme/needs-secret", "llm")
		// The example manifest's API_TOKEN is optional; make it required through the image manifest.
		m := strings.Replace(sdkManifest(t, "llm"), "required: false", "required: true", 1)
		fc.Register("ghcr.io/acme/needs:1", "llm")
		_, err := h.m.Install(ctx, manager.InstallRequest{Type: "llm", Mode: "image", Image: "ghcr.io/acme/needs:1", Manifest: m})
		if err == nil || !strings.Contains(err.Error(), "API_TOKEN") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("contract failure stops the candidate", func(t *testing.T) {
		h, fc := newContainerHarness(t, 0)
		fc.Register("ghcr.io/acme/wrongtype:1", "queue") // a queue image installed as an LLM
		rec, _ := h.m.Install(ctx, manager.InstallRequest{Type: "llm", Mode: "image", Image: "ghcr.io/acme/wrongtype:1", Manifest: sdkManifest(t, "llm")})
		h.m.WaitIdle()
		got, _ := h.m.Get(rec.ID)
		if got.State != plugins.StateFailed || got.Verification == nil || got.Verification.Passed {
			t.Fatalf("state=%s verification=%+v", got.State, got.Verification)
		}
		if !testutil.Wait(2*time.Second, func() bool { return !fc.Running(rec.ID) }) {
			t.Fatal("the failed candidate is still running")
		}
	})
}

func TestReplacedInstanceStopsAfterTheRollbackWindowAndRestartsOnDemand(t *testing.T) {
	h, fc := newContainerHarness(t, 150*time.Millisecond)
	fc.Register("ghcr.io/acme/v1:1", "llm")
	fc.Register("ghcr.io/acme/v2:1", "llm")
	ctx := context.Background()
	install := func(image, version string) string {
		m := strings.Replace(sdkManifest(t, "llm"), "version: 1.0.0", "version: "+version, 1)
		rec, err := h.m.Install(ctx, manager.InstallRequest{Type: "llm", Mode: "image", Image: image, Manifest: m, Config: map[string]any{"model": "m"}})
		if err != nil {
			t.Fatal(err)
		}
		h.waitState(rec.ID, plugins.StateVerified)
		return rec.ID
	}
	id1, id2 := install("ghcr.io/acme/v1:1", "1.0.0"), install("ghcr.io/acme/v2:1", "1.1.0")
	h.mustActivate(id1, registry.Confirmations{})
	h.mustActivate(id2, registry.Confirmations{})

	if !testutil.Wait(3*time.Second, func() bool { return !fc.Running(id1) }) {
		t.Fatal("the replaced plugin's container was not stopped after the rollback window")
	}
	if !fc.Running(id2) {
		t.Fatal("the active plugin's container must keep running")
	}
	// Rolling back after the window restarts the old plugin on demand.
	res, err := h.m.Rollback(ctx, id2, registry.Confirmations{})
	if err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if res.Record.ID != id1 || !fc.Running(id1) {
		t.Fatalf("rollback did not restart the previous plugin (active=%s running=%v)", res.Record.ID, fc.Running(id1))
	}
}

func TestDeleteStopsTheInstance(t *testing.T) {
	h, fc := newContainerHarness(t, 0)
	fc.Register("ghcr.io/acme/v1:1", "llm")
	rec, err := h.m.Install(context.Background(), manager.InstallRequest{Type: "llm", Mode: "image", Image: "ghcr.io/acme/v1:1", Manifest: sdkManifest(t, "llm")})
	if err != nil {
		t.Fatal(err)
	}
	h.waitState(rec.ID, plugins.StateVerified)
	if err := h.m.Delete(context.Background(), rec.ID); err != nil {
		t.Fatal(err)
	}
	if fc.Running(rec.ID) {
		t.Fatal("the instance survived the delete")
	}
	if _, err := h.m.Get(rec.ID); !manager.IsNotFound(err) {
		t.Fatalf("record survived: %v", err)
	}
}
