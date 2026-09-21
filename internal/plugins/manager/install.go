package manager

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/leenkabha/llm_cache/internal/plugins"
	"github.com/leenkabha/llm_cache/internal/plugins/manifest"
	"github.com/leenkabha/llm_cache/internal/plugins/registry"
	"github.com/leenkabha/llm_cache/internal/plugins/secrets"
)

func asInvalid(err error, target **InvalidError) bool { return errors.As(err, target) }

// probeHealth asks an adapter whether it is healthy.
func probeHealth(ctx context.Context, impl any) error {
	switch h := impl.(type) {
	case interface{ Health(context.Context) error }:
		return h.Health(ctx)
	case interface{ Health() error }:
		return h.Health()
	}
	return nil // implementation has no health probe
}

// Install records a plugin and starts the verification pipeline in the
// background. The returned record is in state draft; poll Get to follow it.
func (m *Manager) Install(ctx context.Context, req InstallRequest) (*registry.Record, error) {
	if err := m.requireEnabled(); err != nil {
		return nil, err
	}
	p, err := m.parseRequest(req)
	if err != nil {
		m.audit("install", nil, "denied", err.Error())
		return nil, err
	}
	rec, err := m.newRecord(p, req.Name)
	if err != nil {
		return nil, err
	}
	if err := m.sealInto(rec, p.secrets); err != nil {
		return nil, err
	}
	if req.Confirmations != nil {
		rec.Confirmations = *req.Confirmations
	}
	if err := m.reg.Create(rec); err != nil {
		return nil, err
	}
	m.audit("install", rec, "ok", string(p.mode)+" "+rec.Name+"@"+rec.Version)
	m.logf(rec.ID, "installation requested (%s, %s)", p.mode, p.typ)
	m.startPipeline(rec.ID, req.Activate)
	return rec, nil
}

func (m *Manager) newRecord(p *parsedRequest, hintName string) (*registry.Record, error) {
	now := time.Now().UTC()
	rec := &registry.Record{
		ID: newID(), Type: p.typ, ContractVersion: plugins.ContractVersion, Mode: p.mode,
		Source: p.source, Manifest: p.manifest, Config: p.config,
		State: plugins.StateDraft, Health: registry.Health{Status: "unknown"},
		CreatedAt: now, UpdatedAt: now,
	}
	if p.manifest != nil {
		rec.Name, rec.Version = p.manifest.Metadata.Name, p.manifest.Metadata.Version
		if cfg, err := p.manifest.ResolveConfig(p.config); err == nil {
			rec.Config = cfg
		}
	} else {
		rec.Name, rec.Version = placeholderName(hintName, p.source), "0.0.0"
	}
	return rec, nil
}

func placeholderName(hint string, s registry.Source) string {
	n := strings.ToLower(hint)
	if n == "" {
		switch {
		case s.RepoURL != "":
			n = s.RepoURL[strings.LastIndex(s.RepoURL, "/")+1:]
		case s.ImageRef != "":
			n = strings.SplitN(s.ImageRef[strings.LastIndex(s.ImageRef, "/")+1:], ":", 2)[0]
			n = strings.SplitN(n, "@", 2)[0]
		}
	}
	n = strings.TrimSuffix(n, ".git")
	b := strings.Builder{}
	for _, r := range n {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		out = "plugin"
	}
	if len(out) > 60 {
		out = out[:60]
	}
	return out
}

// sealInto encrypts every supplied secret into the record.
func (m *Manager) sealInto(rec *registry.Record, sec map[string]string) error {
	if len(sec) == 0 {
		return nil
	}
	rec.Secrets = map[string]secrets.Sealed{}
	for name, val := range sec {
		s, err := m.box.Seal(rec.ID, name, val)
		if err != nil {
			return fmt.Errorf("encrypt secret %s: %w", name, err)
		}
		rec.Secrets[name] = s
	}
	return nil
}

func (m *Manager) startPipeline(id string, activate bool) {
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), m.cfg.BuildTimeout+m.cfg.HealthTimeout+5*time.Minute)
		defer cancel()
		if err := m.pipeline(ctx, id); err != nil {
			return // pipeline has already recorded the failure
		}
		if activate {
			rec, err := m.reg.Get(id)
			if err != nil {
				return
			}
			actx, acancel := context.WithTimeout(context.Background(), m.cfg.ActivationTimeout)
			defer acancel()
			if _, err := m.activate(actx, rec.ID, rec.Confirmations); err != nil {
				m.logf(id, "automatic activation did not complete: %v", err)
			}
		}
	}()
}

// fail records a pipeline failure and releases the candidate instance.
func (m *Manager) fail(id string, cause error, sec map[string]string) error {
	msg := secrets.Scrub(cause.Error(), secretValues(sec)...)
	m.logf(id, "FAILED: %s", msg)
	if rec, err := m.reg.Get(id); err == nil {
		m.audit("install", rec, "failed", msg)
		if rec.Mode != plugins.ModeEndpoint && m.ctl != nil {
			sctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			_ = m.ctl.Stop(sctx, id)
			cancel()
		}
	}
	_, _ = m.reg.Update(id, func(r *registry.Record) error {
		r.State, r.StateDetail = plugins.StateFailed, msg
		r.Health = registry.Health{Status: "unknown"}
		return nil
	})
	return cause
}

// pipeline validates, prepares, starts, tests and health-checks a candidate,
// leaving it in state verified (or failed).
func (m *Manager) pipeline(ctx context.Context, id string) error {
	rec, err := m.reg.Get(id)
	if err != nil {
		return err
	}
	sec, err := m.openSecrets(rec)
	if err != nil {
		return m.fail(id, err, nil)
	}

	if _, err := m.setState(id, plugins.StateValidating, "validating manifest"); err != nil {
		return err
	}
	m.logf(id, "validating manifest and configuration")

	// Image and repository plugins: build/pull, scan, pin the digest.
	if rec.Mode != plugins.ModeEndpoint {
		st := plugins.StateScanning
		if rec.Mode == plugins.ModeRepository {
			st = plugins.StateBuilding
		}
		if _, err := m.setState(id, st, "preparing image"); err != nil {
			return err
		}
		pr, err := m.ctl.Prepare(ctx, PrepareRequest{
			Mode: rec.Mode, Type: rec.Type, Image: rec.Source.ImageRef, Repo: rec.Source.RepoURL,
			Revision: rec.Source.Revision, PluginID: rec.ID, Manifest: manifestText(rec),
		})
		if err != nil {
			return m.fail(id, fmt.Errorf("prepare: %w", err), sec)
		}
		for _, l := range pr.Log {
			m.logf(id, "controller: %s", l)
		}
		mf, err := manifest.Parse([]byte(pr.Manifest))
		if err != nil {
			return m.fail(id, err, sec)
		}
		if mf.Spec.Type != string(rec.Type) {
			return m.fail(id, fmt.Errorf("manifest is for type %s but the plugin was installed as %s", mf.Spec.Type, rec.Type), sec)
		}
		cfg, err := mf.ResolveConfig(rec.Config)
		if err != nil {
			return m.fail(id, err, sec)
		}
		names := make([]string, 0, len(sec))
		for n := range sec {
			names = append(names, n)
		}
		if err := mf.CheckSecrets(names); err != nil {
			return m.fail(id, err, sec)
		}
		if pr.Scan != nil && pr.Scan.Status == "failed" {
			return m.fail(id, fmt.Errorf("image scan failed: %s", pr.Scan.Message), sec)
		}
		if _, err := m.reg.Update(id, func(r *registry.Record) error {
			r.Manifest, r.Name, r.Version, r.Config = mf, mf.Metadata.Name, mf.Metadata.Version, cfg
			r.ImageDigest, r.Commit, r.Scan, r.Provenance = pr.Digest, pr.Commit, pr.Scan, pr.Provenance
			return nil
		}); err != nil {
			return err
		}
		if _, err := m.setState(id, plugins.StateScanning, "scanned"); err != nil && rec.Mode == plugins.ModeRepository {
			return err
		}
		m.logf(id, "image pinned to %s", pr.Digest)
		rec, _ = m.reg.Get(id)
	}

	// Start the candidate (containers) and run the contract suite.
	if rec.Mode != plugins.ModeEndpoint {
		if _, err := m.setState(id, plugins.StateStarting, "starting isolated candidate"); err != nil {
			return err
		}
		m.logf(id, "starting candidate container")
		sctx, scancel := context.WithTimeout(ctx, m.cfg.HealthTimeout+30*time.Second)
		_, err := m.ensureInstance(sctx, rec, sec)
		scancel()
		if err != nil {
			return m.fail(id, err, sec)
		}
	}
	if _, err := m.setState(id, plugins.StateTesting, "running contract tests"); err != nil {
		return err
	}
	v, err := m.runContract(ctx, rec, sec)
	if err != nil {
		return m.fail(id, fmt.Errorf("contract tests could not run: %w", err), sec)
	}
	_, _ = m.reg.Update(id, func(r *registry.Record) error { r.Verification = v; return nil })
	m.logf(id, "%s", v.Summary)
	if !v.Passed {
		return m.fail(id, errors.New(v.Summary), sec)
	}

	if _, err := m.setState(id, plugins.StateChecking, "checking health"); err != nil {
		return err
	}
	hctx, cancel := context.WithTimeout(ctx, m.cfg.HealthTimeout)
	defer cancel()
	adapter, err := m.buildAdapter(hctx, rec, sec)
	if err != nil {
		return m.fail(id, err, sec)
	}
	if err := probeHealth(hctx, adapter); err != nil {
		closeAdapter(adapter)
		return m.fail(id, fmt.Errorf("health check failed: %w", err), sec)
	}
	closeAdapter(adapter)

	_, err = m.reg.Update(id, func(r *registry.Record) error {
		r.Health = registry.Health{Status: "healthy", CheckedAt: time.Now().UTC()}
		r.InstanceID = ""
		if rec.Mode != plugins.ModeEndpoint {
			r.InstanceID = r.ID
		}
		return nil
	})
	if err != nil {
		return err
	}
	if _, err := m.setState(id, plugins.StateVerified, "verified: contract tests and health check passed"); err != nil {
		return err
	}
	m.logf(id, "verified; ready to activate")
	return nil
}

func manifestText(rec *registry.Record) string {
	if rec.Manifest == nil || rec.Mode == plugins.ModeRepository {
		return ""
	}
	return marshalManifest(rec.Manifest)
}

// semver compares two versions by major.minor.patch, treating a pre-release as
// lower than the same release. It returns -1, 0 or 1.
func semverCompare(a, b string) int {
	parse := func(s string) (nums [3]int, pre string) {
		s = strings.SplitN(s, "+", 2)[0]
		if i := strings.Index(s, "-"); i >= 0 {
			s, pre = s[:i], s[i+1:]
		}
		for i, part := range strings.SplitN(s, ".", 3) {
			nums[i], _ = strconv.Atoi(part)
		}
		return
	}
	an, ap := parse(a)
	bn, bp := parse(b)
	for i := 0; i < 3; i++ {
		if an[i] != bn[i] {
			if an[i] < bn[i] {
				return -1
			}
			return 1
		}
	}
	switch {
	case ap == bp:
		return 0
	case ap == "":
		return 1
	case bp == "":
		return -1
	case ap < bp:
		return -1
	}
	return 1
}
