package manager

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/leenkabha/llm_cache/internal/plugins"
	"github.com/leenkabha/llm_cache/internal/plugins/registry"
	"github.com/leenkabha/llm_cache/internal/plugins/secrets"
)

// Upgrade installs a new version of an existing plugin as a separate record
// (same name and type, higher version). The old version keeps running; when the
// upgrade is activated it becomes the rollback target.
func (m *Manager) Upgrade(ctx context.Context, id string, req InstallRequest) (*registry.Record, error) {
	if err := m.requireEnabled(); err != nil {
		return nil, err
	}
	old, err := m.reg.Get(id)
	if err != nil {
		return nil, err
	}
	if req.Type == "" {
		req.Type = string(old.Type)
	}
	if req.Type != string(old.Type) {
		return nil, invalidf("an upgrade cannot change the plugin type (%s -> %s)", old.Type, req.Type)
	}
	if req.Name == "" {
		req.Name = old.Name
	}
	if req.Mode == "" {
		req.Mode = string(old.Mode)
	}
	if req.Config == nil {
		req.Config = old.Config // keep the existing non-secret configuration
	}
	// Secrets not re-entered are carried over: decrypted here, re-sealed below
	// under the new record's ID, never leaving this function.
	carried, err := m.openSecrets(old)
	if err != nil {
		return nil, err
	}
	merged := map[string]string{}
	for k, v := range carried {
		merged[k] = v
	}
	for k, v := range req.Secrets {
		merged[k] = v
	}
	req.Secrets = merged

	p, err := m.parseRequest(req)
	if err != nil {
		m.audit("upgrade", old, "denied", err.Error())
		return nil, err
	}
	if p.manifest != nil {
		if p.manifest.Metadata.Name != old.Name {
			return nil, invalidf("an upgrade must keep the plugin name (%q, not %q)", old.Name, p.manifest.Metadata.Name)
		}
		if semverCompare(p.manifest.Metadata.Version, old.Version) <= 0 {
			return nil, invalidf("version %s is not newer than the installed %s", p.manifest.Metadata.Version, old.Version)
		}
	}
	rec, err := m.newRecord(p, old.Name)
	if err != nil {
		return nil, err
	}
	rec.UpgradedFromID = old.ID
	if err := m.sealInto(rec, p.secrets); err != nil {
		return nil, err
	}
	if req.Confirmations != nil {
		rec.Confirmations = *req.Confirmations
	}
	if err := m.reg.Create(rec); err != nil {
		return nil, err
	}
	m.audit("upgrade", rec, "ok", fmt.Sprintf("%s -> %s", old.Version, rec.Version))
	m.logf(rec.ID, "upgrade of %s (%s) requested", old.Name, old.Version)
	m.startPipeline(rec.ID, req.Activate)
	return rec, nil
}

// Delete removes an inactive plugin: its instance, encrypted secrets and record.
// The active plugin, and the plugin an active one would roll back to, cannot be
// deleted.
func (m *Manager) Delete(ctx context.Context, id string) error {
	if err := m.requireEnabled(); err != nil {
		return err
	}
	rec, err := m.reg.Get(id)
	if err != nil {
		return err
	}
	if rec.Active {
		m.audit("delete", rec, "denied", "plugin is active")
		return invalidf("plugin is active; deactivate it first")
	}
	if rec.State.InProgress() {
		return invalidf("plugin is %s; wait for it to finish", rec.State)
	}
	all, err := m.reg.List()
	if err != nil {
		return err
	}
	for _, o := range all {
		if o.Active && o.PreviousActiveID == id {
			m.audit("delete", rec, "denied", "rollback target of "+o.ID)
			return invalidf("plugin is the rollback target of the active plugin %q; deactivate that plugin or activate another version first", o.Name)
		}
	}
	m.stateMu.Lock()
	if t := m.timers[id]; t != nil {
		t.Stop()
		delete(m.timers, id)
	}
	delete(m.owned, id)
	m.stateMu.Unlock()
	if rec.Mode != plugins.ModeEndpoint && m.ctl != nil {
		sctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		if err := m.ctl.Stop(sctx, id); err != nil {
			m.logf(id, "could not stop instance while deleting: %v", err)
		}
		cancel()
	}
	if err := m.reg.Delete(id); err != nil {
		return err
	}
	m.audit("delete", rec, "ok", rec.Name+"@"+rec.Version)
	return nil
}

// Restore re-activates the persisted selection at startup. It must run BEFORE
// the orchestrator is constructed, because the orchestrator rebuilds the vector
// store and eviction policy from persistence during startup and must do so
// against the restored components.
//
// A state-bearing component (embedder, vector store, persistence) that cannot be
// restored is an error: silently falling back to the built-in would serve a
// different cache than the administrator selected. Stateless components (LLM,
// queue, policy) fall back to the built-in and the record is marked failed.
func (m *Manager) Restore(ctx context.Context) error {
	if !m.cfg.Enabled || m.box == nil {
		return nil
	}
	active, err := m.reg.Active()
	if err != nil {
		return fmt.Errorf("read active plugin selection: %w", err)
	}
	for _, slot := range slotTypes {
		id := active[slot]
		if id == "" {
			continue
		}
		rec, err := m.reg.Get(id)
		if err != nil {
			log.Printf("plugin restore: active %s plugin %s is missing from the registry; clearing the selection", slot, id)
			_ = m.reg.ClearActive(slot)
			continue
		}
		if err := m.restoreWithRetry(ctx, slot, rec); err != nil {
			msg := secrets.Scrub(err.Error())
			m.logf(id, "RESTORE FAILED: %s", msg)
			if stateBearing(slot) {
				return fmt.Errorf("cannot restore the active %s plugin %q (%s): %s -- fix it, or deactivate the plugin in the registry to fall back to the built-in", slot, rec.Name, id, msg)
			}
			_, _ = m.reg.Update(id, func(r *registry.Record) error {
				r.Health = registry.Health{Status: "unhealthy", Message: msg, CheckedAt: time.Now().UTC()}
				r.Active, r.State, r.StateDetail = false, plugins.StateFailed, "could not be restored at startup: "+msg
				return nil
			})
			_ = m.reg.ClearActive(slot)
			m.audit("restore", rec, "failed", msg)
			continue
		}
		m.stateMu.Lock()
		m.active[slot] = id
		m.stateMu.Unlock()
		if slot == plugins.TypeVectorStore && rec.Confirmations.Threshold != nil {
			t := *rec.Confirmations.Threshold
			m.restoredThreshold = &t
		}
		m.logf(id, "restored as the active %s implementation", slot)
		m.audit("restore", rec, "ok", "")
	}
	return nil
}

// restoreWithRetry tolerates the controller or a hosted plugin coming up a little
// after the orchestrator (a whole-stack restart starts everything at once). It
// gives up early for errors retrying cannot fix, such as a wrong master key.
func (m *Manager) restoreWithRetry(ctx context.Context, slot plugins.Type, rec *registry.Record) error {
	deadline := time.Now().Add(m.cfg.RestoreWait)
	for {
		err := m.restoreOne(ctx, slot, rec)
		if err == nil {
			return nil
		}
		if errors.Is(err, secrets.ErrKeyMismatch) || errors.Is(err, secrets.ErrCorrupt) || time.Now().After(deadline) || ctx.Err() != nil {
			return err
		}
		m.logf(rec.ID, "restore attempt failed (%v); retrying", secrets.Scrub(err.Error()))
		select {
		case <-time.After(2 * time.Second):
		case <-ctx.Done():
			return err
		}
	}
}

func (m *Manager) restoreOne(ctx context.Context, slot plugins.Type, rec *registry.Record) error {
	m.logf(rec.ID, "restoring the active %s plugin %q", slot, rec.Name)
	sec, err := m.openSecrets(rec)
	if err != nil {
		return err
	}
	rctx, cancel := context.WithTimeout(ctx, m.cfg.HealthTimeout+30*time.Second)
	defer cancel()
	impl, err := m.buildAdapter(rctx, rec, sec)
	if err != nil {
		return err
	}
	if err := probeHealth(rctx, impl); err != nil {
		closeAdapter(impl)
		return fmt.Errorf("not healthy: %w", err)
	}
	if _, err := m.slots.swap(slot, impl); err != nil {
		closeAdapter(impl)
		return err
	}
	m.stateMu.Lock()
	m.owned[rec.ID] = impl
	m.stateMu.Unlock()
	return nil
}

// RestoredThreshold returns the similarity threshold confirmed when the active
// vector-store plugin was activated, so the orchestrator can apply it at startup.
func (m *Manager) RestoredThreshold() *float64 { return m.restoredThreshold }

// StartMonitor probes every active plugin periodically and records its health.
func (m *Manager) StartMonitor(ctx context.Context, interval time.Duration) {
	if !m.cfg.Enabled {
		return
	}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				m.CheckActiveHealth(ctx)
			}
		}
	}()
}

// CheckActiveHealth probes each active plugin once.
func (m *Manager) CheckActiveHealth(ctx context.Context) {
	for slot, id := range m.ActiveIDs() {
		if id == "" {
			continue
		}
		hctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := probeHealth(hctx, m.slots.current(slot))
		cancel()
		h := registry.Health{Status: "healthy", CheckedAt: time.Now().UTC()}
		if err != nil {
			h.Status, h.Message = "unhealthy", secrets.Scrub(err.Error())
		}
		prev, gerr := m.reg.Get(id)
		if gerr != nil {
			continue
		}
		if prev.Health.Status != h.Status {
			m.logf(id, "health changed: %s -> %s %s", prev.Health.Status, h.Status, h.Message)
		}
		_, _ = m.reg.Update(id, func(r *registry.Record) error { r.Health = h; return nil })
	}
}

// Close stops background work.
func (m *Manager) Close() {
	m.stateMu.Lock()
	for _, t := range m.timers {
		t.Stop()
	}
	m.stateMu.Unlock()
	m.WaitIdle()
}

// IsNotFound reports a missing-record error.
func IsNotFound(err error) bool { return errors.Is(err, registry.ErrNotFound) }
