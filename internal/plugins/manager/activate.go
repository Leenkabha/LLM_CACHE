package manager

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/leenkabha/llm_cache/internal/cachequeue"
	"github.com/leenkabha/llm_cache/internal/embedder"
	"github.com/leenkabha/llm_cache/internal/persistence"
	"github.com/leenkabha/llm_cache/internal/plugins"
	"github.com/leenkabha/llm_cache/internal/plugins/registry"
	"github.com/leenkabha/llm_cache/internal/plugins/secrets"
	"github.com/leenkabha/llm_cache/internal/policy"
	"github.com/leenkabha/llm_cache/internal/vectorstore"
)

// Result is the outcome of an activation, deactivation or rollback.
type Result struct {
	Record   *registry.Record `json:"-"`
	Warnings []string         `json:"warnings,omitempty"`
}

type switchMode int

const (
	modeActivate switchMode = iota
	modeDeactivate
	modeRollback
)

// plan is what a switch must do, beyond swapping the pointer, to keep the cache
// consistent. Nothing in a plan touches state until execute runs it.
type plan struct {
	needGate  bool
	forceMiss bool // answer every query as a miss while state is inconsistent
	migrate   func(ctx context.Context) error
	after     func(ctx context.Context) error // runs after the swap; failure reverts it
	cleanup   func()                          // undo migrate side effects on failure
	warnings  []string
	threshold *float64
}

// Activate makes a verified plugin the active implementation of its component.
func (m *Manager) Activate(ctx context.Context, id string, conf registry.Confirmations) (*Result, error) {
	if err := m.requireEnabled(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, m.cfg.ActivationTimeout)
	defer cancel()
	return m.activate(ctx, id, conf)
}

func (m *Manager) activate(ctx context.Context, id string, conf registry.Confirmations) (*Result, error) {
	rec, err := m.reg.Get(id)
	if err != nil {
		return nil, err
	}
	switch rec.State {
	case plugins.StateVerified, plugins.StateInactive, plugins.StateRolledBack:
	case plugins.StateFailed:
		if rec.Verification == nil || !rec.Verification.Passed {
			return nil, invalidf("plugin failed verification and cannot be activated; see its log")
		}
	case plugins.StateActive:
		return nil, invalidf("plugin is already active")
	default:
		return nil, invalidf("plugin is %s; wait for verification to finish", rec.State)
	}
	return m.switchSlot(ctx, rec.Type.Slot(), rec, conf, modeActivate)
}

// Deactivate returns the component to its built-in implementation.
func (m *Manager) Deactivate(ctx context.Context, id string, conf registry.Confirmations) (*Result, error) {
	if err := m.requireEnabled(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, m.cfg.ActivationTimeout)
	defer cancel()
	rec, err := m.reg.Get(id)
	if err != nil {
		return nil, err
	}
	if !rec.Active {
		return nil, invalidf("plugin is not active")
	}
	return m.switchSlot(ctx, rec.Type.Slot(), rec, conf, modeDeactivate)
}

// Rollback re-activates whatever was active before this plugin.
func (m *Manager) Rollback(ctx context.Context, id string, conf registry.Confirmations) (*Result, error) {
	if err := m.requireEnabled(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, m.cfg.ActivationTimeout)
	defer cancel()
	rec, err := m.reg.Get(id)
	if err != nil {
		return nil, err
	}
	if !rec.Active {
		return nil, invalidf("only the active plugin can be rolled back")
	}
	return m.switchSlot(ctx, rec.Type.Slot(), rec, conf, modeRollback)
}

// switchSlot is the single path by which any component changes implementation.
// rec is the plugin being activated (modeActivate) or the ACTIVE plugin being
// left (modeDeactivate, modeRollback).
func (m *Manager) switchSlot(ctx context.Context, slot plugins.Type, rec *registry.Record, conf registry.Confirmations, mode switchMode) (*Result, error) {
	lock := m.slotMu[slot]
	if !lock.TryLock() {
		return nil, ErrBusy
	}
	defer lock.Unlock()
	host, err := m.getHost()
	if err != nil {
		return nil, err
	}

	// Decide the target: a record, or nil for the built-in implementation.
	var target *registry.Record
	switch mode {
	case modeActivate:
		target = rec
	case modeRollback:
		if rec.PreviousActiveID != "" {
			if target, err = m.reg.Get(rec.PreviousActiveID); err != nil {
				return nil, invalidf("the previous plugin no longer exists; deactivate instead")
			}
		}
	}
	oldID := m.ActiveIDs()[slot]
	if mode == modeActivate && oldID == rec.ID {
		return nil, invalidf("plugin is already active")
	}
	if mode != modeActivate && conf.Threshold == nil && rec.PreviousThreshold != nil {
		// Leaving a plugin that changed the similarity threshold: put it back.
		t := *rec.PreviousThreshold
		conf.Threshold = &t
	}
	actor := rec // the record whose state we report progress on
	prevState := actor.State

	revert := func() {
		_, _ = m.reg.Update(actor.ID, func(r *registry.Record) error {
			r.State = prevState
			return nil
		})
	}
	failed := func(cause error) (*Result, error) {
		var req *RequirementError
		var inc *IncompatibleError
		switch {
		case errors.As(cause, &req):
			revert() // waiting for the administrator: not a failure
		case mode == modeRollback:
			_, _ = m.reg.Update(actor.ID, func(r *registry.Record) error {
				r.State, r.StateDetail = plugins.StateActive, "rollback failed: "+secrets.Scrub(cause.Error())
				return nil
			})
		default:
			detail := secrets.Scrub(cause.Error())
			if errors.As(cause, &inc) {
				detail = inc.Reason
			}
			_, _ = m.reg.Update(actor.ID, func(r *registry.Record) error {
				r.State, r.StateDetail = plugins.StateFailed, detail
				return nil
			})
		}
		m.logf(actor.ID, "%s not completed: %v (previous implementation left in place)", modeName(mode), cause)
		m.audit(modeName(mode), actor, "failed", cause.Error())
		return nil, cause
	}

	first := plugins.StateChecking
	if mode == modeRollback {
		first = plugins.StateRollingBack
	}
	if mode != modeDeactivate {
		if _, err := m.setState(actor.ID, first, "preparing candidate"); err != nil {
			return nil, err
		}
	}

	// 1. Prepare the candidate implementation and check its health.
	var impl any
	owned := false
	if target == nil {
		impl = m.slots.builtin(slot)
	} else {
		sec, err := m.openSecrets(target)
		if err != nil {
			return failed(err)
		}
		impl, err = m.buildAdapter(ctx, target, sec)
		if err != nil {
			return failed(err)
		}
		owned = true
		hctx, hcancel := context.WithTimeout(ctx, m.cfg.HealthTimeout)
		err = probeHealth(hctx, impl)
		hcancel()
		if err != nil {
			closeAdapter(impl)
			return failed(fmt.Errorf("candidate is not healthy: %w", err))
		}
	}
	discard := func() {
		if owned {
			closeAdapter(impl)
		}
	}

	// 2. Work out what the switch needs and whether the administrator must confirm.
	pl, err := m.planFor(ctx, slot, target, impl, conf, host)
	if err != nil {
		discard()
		return failed(err)
	}

	prevThreshold := host.Threshold()

	// 3. Execute: pause writes, migrate, swap, post-check.
	if pl.migrate != nil || pl.needGate {
		if _, err := m.setState(actor.ID, plugins.StateMigrating, "migrating cache state"); err != nil && mode != modeRollback && mode != modeDeactivate {
			discard()
			return failed(err)
		}
	}
	prevImpl, err := m.execute(ctx, slot, impl, pl, host)
	if err != nil {
		discard()
		return failed(err)
	}
	if mode != modeDeactivate {
		_, _ = m.reg.Update(actor.ID, func(r *registry.Record) error {
			if plugins.ValidTransition(r.State, plugins.StateActivating) {
				r.State, r.StateDetail = plugins.StateActivating, "activating"
			}
			return nil
		})
	}

	// 4. Commit the registry. If that fails, undo the swap: memory and registry
	// must never disagree about which plugin is active.
	if err := m.commit(slot, oldID, actor, target, conf, pl, mode, prevThreshold); err != nil {
		if _, serr := m.slots.swap(slot, prevImpl); serr != nil {
			m.logf(actor.ID, "CRITICAL: could not undo swap after registry failure: %v", serr)
		}
		discard()
		return failed(fmt.Errorf("could not record the switch: %w", err))
	}
	if pl.threshold != nil {
		host.SetThreshold(*pl.threshold)
	}
	m.afterSwitch(slot, oldID, prevImpl)

	res := &Result{Warnings: pl.warnings}
	cur := actor.ID
	if target != nil {
		cur = target.ID
	}
	res.Record, _ = m.reg.Get(cur)
	for _, w := range pl.warnings {
		m.logf(cur, "warning: %s", w)
	}
	m.logf(cur, "%s complete", modeName(mode))
	m.audit(modeName(mode), res.Record, "ok", strings.Join(pl.warnings, "; "))
	return res, nil
}

func modeName(m switchMode) string {
	switch m {
	case modeDeactivate:
		return "deactivate"
	case modeRollback:
		return "rollback"
	}
	return "activate"
}

// execute runs the plan and performs the atomic swap. On error the previous
// implementation is still installed.
func (m *Manager) execute(ctx context.Context, slot plugins.Type, impl any, pl *plan, host Host) (any, error) {
	if pl.needGate {
		gctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		resume, err := host.PauseWrites(gctx)
		cancel()
		if err != nil {
			return nil, err
		}
		defer resume()
	}
	if pl.forceMiss {
		saved := host.Threshold()
		host.SetThreshold(-1) // no query can match while vectors and embedder disagree
		defer host.SetThreshold(saved)
	}
	if pl.migrate != nil {
		if err := pl.migrate(ctx); err != nil {
			if pl.cleanup != nil {
				pl.cleanup()
			}
			return nil, err
		}
	}
	prev, err := m.slots.swap(slot, impl)
	if err != nil {
		if pl.cleanup != nil {
			pl.cleanup()
		}
		return nil, err
	}
	if pl.after != nil {
		if err := pl.after(ctx); err != nil {
			if _, serr := m.slots.swap(slot, prev); serr != nil {
				return nil, fmt.Errorf("%w (and the swap could not be undone: %v)", err, serr)
			}
			if pl.cleanup != nil {
				pl.cleanup()
			}
			return nil, err
		}
	}
	return prev, nil
}

// commit records the switch in the registry.
func (m *Manager) commit(slot plugins.Type, oldID string, actor, target *registry.Record, conf registry.Confirmations, pl *plan, mode switchMode, prevThreshold float64) error {
	newID := ""
	if target != nil {
		newID = target.ID
	}
	if newID == "" {
		if err := m.reg.ClearActive(slot); err != nil {
			return err
		}
	} else if err := m.reg.SetActive(slot, newID); err != nil {
		return err
	}

	if newID != "" {
		conf2 := conf
		if pl.threshold != nil {
			t := *pl.threshold
			conf2.Threshold = &t
		}
		_, err := m.reg.Update(newID, func(r *registry.Record) error {
			r.State, r.StateDetail, r.Active = plugins.StateActive, "", true
			r.Confirmations = conf2
			if mode == modeActivate {
				r.PreviousActiveID = oldID
				r.PreviousThreshold = nil
				if pl.threshold != nil {
					pt := prevThreshold
					r.PreviousThreshold = &pt
				}
			} else {
				r.PreviousActiveID, r.PreviousThreshold = "", nil
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	if oldID != "" && oldID != newID {
		final := plugins.StateInactive
		if mode == modeRollback {
			final = plugins.StateRolledBack
		}
		_, _ = m.reg.Update(oldID, func(r *registry.Record) error {
			r.Active = false
			r.State, r.StateDetail = final, ""
			return nil
		})
	}
	m.stateMu.Lock()
	m.active[slot] = newID
	m.stateMu.Unlock()
	return nil
}

// afterSwitch retires the replaced implementation: adapters this manager built
// are closed (the queue keeps its old adapter until drained), and the replaced
// plugin's instance is stopped after the rollback window.
func (m *Manager) afterSwitch(slot plugins.Type, oldID string, prevImpl any) {
	m.stateMu.Lock()
	owned := m.owned[oldID]
	delete(m.owned, oldID)
	// The newly active plugin must not be stopped by an earlier rollback timer.
	if t := m.timers[m.active[slot]]; t != nil {
		t.Stop()
		delete(m.timers, m.active[slot])
	}
	m.stateMu.Unlock()

	if slot == plugins.TypeQueue {
		q, ok := prevImpl.(cachequeue.Queue)
		if ok {
			m.wg.Add(1)
			go func() {
				defer m.wg.Done()
				left := m.slots.Queue.Drain(context.Background(), q, m.cfg.QueueDrainWindow)
				switch {
				case left == 0:
					m.logf(oldID, "old queue drained")
				case left > 0:
					m.logf(oldID, "old queue stopped with %d unprocessed job(s) still stored in it; they are processed again if it is reactivated", left)
				default:
					m.logf(oldID, "old queue worker stopped after the drain window (backlog unknown)")
				}
				if owned != nil {
					closeAdapter(prevImpl)
				}
			}()
			m.scheduleStop(oldID)
			return
		}
	}
	if owned != nil {
		closeAdapter(prevImpl)
	}
	m.scheduleStop(oldID)
}

func (m *Manager) scheduleStop(id string) {
	if id == "" || m.ctl == nil {
		return
	}
	rec, err := m.reg.Get(id)
	if err != nil || rec.Mode == plugins.ModeEndpoint {
		return
	}
	m.stateMu.Lock()
	defer m.stateMu.Unlock()
	if old := m.timers[id]; old != nil {
		old.Stop()
	}
	m.timers[id] = time.AfterFunc(m.cfg.RollbackWindow, func() {
		r, err := m.reg.Get(id)
		if err != nil || r.Active {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := m.ctl.Stop(ctx, id); err != nil {
			m.logf(id, "could not stop instance after the rollback window: %v", err)
			return
		}
		m.logf(id, "instance stopped after the rollback window; it is restarted on demand")
	})
}

// ---- planning ----------------------------------------------------------------

func (m *Manager) planFor(ctx context.Context, slot plugins.Type, target *registry.Record, impl any, conf registry.Confirmations, host Host) (*plan, error) {
	switch slot {
	case plugins.TypeLLM:
		return m.planLLM(target), nil
	case plugins.TypeEmbedder:
		return m.planEmbedder(ctx, impl, conf, host)
	case plugins.TypeVectorStore:
		return m.planVectorStore(ctx, target, impl, conf, host)
	case plugins.TypePersistence:
		return m.planPersistence(ctx, impl, conf, host)
	case plugins.TypeQueue:
		return &plan{}, nil
	case plugins.TypePolicy:
		return m.planPolicy(ctx, impl)
	}
	return nil, fmt.Errorf("unknown slot %s", slot)
}

func (m *Manager) planLLM(target *registry.Record) *plan {
	pl := &plan{}
	if target != nil {
		pl.warnings = append(pl.warnings, "while an LLM plugin is active the built-in provider and LLM_FALLBACK_MODE are bypassed; cached replies stay valid")
	}
	return pl
}

func sortedEntries(s persistence.Store) ([]persistence.Entry, error) {
	entries, err := s.List()
	if err != nil {
		return nil, err
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if !entries[i].CreatedAt.Equal(entries[j].CreatedAt) {
			return entries[i].CreatedAt.Before(entries[j].CreatedAt)
		}
		return entries[i].ID < entries[j].ID
	})
	return entries, nil
}

func (m *Manager) planPolicy(ctx context.Context, impl any) (*plan, error) {
	cand, ok := impl.(policy.EvictionPolicy)
	if !ok {
		return nil, fmt.Errorf("candidate is not an eviction policy")
	}
	pl := &plan{needGate: true}
	pl.warnings = append(pl.warnings, "recency and frequency history is not persisted; the new policy starts from the persisted insertion order")
	pl.migrate = func(ctx context.Context) error {
		entries, err := sortedEntries(m.slots.Store)
		if err != nil {
			return fmt.Errorf("list persisted entries for replay: %w", err)
		}
		cand.Flush()
		ids := make(map[string]bool, len(entries))
		for _, e := range entries { // creation order
			cand.OnInsert(e.ID)
			ids[e.ID] = true
		}
		v, ok := cand.Victim() // also flushes the adapter's pending events
		switch {
		case len(entries) == 0 && ok:
			return fmt.Errorf("policy replay failed: it reports victim %q for an empty cache", v)
		case len(entries) > 0 && !ok:
			return fmt.Errorf("policy replay failed: it has no victim after replaying %d entries", len(entries))
		case ok && !ids[v]:
			return fmt.Errorf("policy replay failed: victim %q is not a persisted entry", v)
		}
		return nil
	}
	pl.cleanup = func() { cand.Flush() }
	return pl, nil
}

func (m *Manager) planEmbedder(ctx context.Context, impl any, conf registry.Confirmations, host Host) (*plan, error) {
	cand, ok := impl.(embedder.Embedder)
	if !ok {
		return nil, fmt.Errorf("candidate is not an embedder")
	}
	mp, ok := impl.(embedder.ModelInfoProvider)
	if !ok {
		return nil, &IncompatibleError{"the candidate embedder cannot report its model identity, so vector compatibility cannot be checked"}
	}
	ci, err := mp.ModelInfo(ctx)
	if err != nil {
		return nil, &IncompatibleError{"the candidate embedder did not report its model: " + err.Error()}
	}
	if vi, err := m.slots.VectorStore.Info(ctx); err == nil && vi.Dim > 0 && vi.Dim != ci.Dim {
		return nil, &IncompatibleError{fmt.Sprintf("the candidate embeds %d-dimensional vectors but the active vector store holds %d-dimensional vectors; activate a vector store with dimension %d first", ci.Dim, vi.Dim, ci.Dim)}
	}
	cur, curErr := m.slots.Embedder.ModelInfo(ctx)
	size, err := host.CacheSize()
	if err != nil {
		return nil, fmt.Errorf("read cache size: %w", err)
	}
	same := curErr == nil && cur == ci
	pl := &plan{}
	if same {
		return pl, nil
	}
	if size == 0 {
		pl.warnings = append(pl.warnings, fmt.Sprintf("embedding model changes to %q (dim %d); the cache is empty so nothing needs converting", ci.Name, ci.Dim))
		return pl, nil
	}
	curDesc := "an unknown model"
	if curErr == nil {
		curDesc = fmt.Sprintf("%q (dim %d)", cur.Name, cur.Dim)
	}
	switch conf.EmbedderCompat {
	case "flush":
		pl.needGate = true
		pl.migrate = func(ctx context.Context) error { return host.FlushCache(ctx) }
		pl.warnings = append(pl.warnings, fmt.Sprintf("the cache (%d entries) was flushed because the embedding model changed", size))
	case "reembed":
		pl.needGate, pl.forceMiss = true, true
		pl.migrate = m.reembed(cand, ci, size)
		pl.warnings = append(pl.warnings, fmt.Sprintf("re-embedded %d cached prompts with the new model", size))
	default:
		return nil, &RequirementError{
			Field: "embedder_compat", Options: []string{"flush", "reembed"},
			Message: fmt.Sprintf("the candidate embeds with model %q (dim %d) but the %d cached vectors came from %s; mixing vector spaces would corrupt cache hits. Choose flush (delete the cache) or reembed (recompute every vector from the stored prompts)", ci.Name, ci.Dim, size, curDesc),
			Warning: "flush permanently deletes all cached replies; reembed keeps them but calls the new embedder once per entry",
		}
	}
	return pl, nil
}

// reembed recomputes every persisted vector with the candidate and rebuilds the
// vector store. It stages everything in memory first and restores the old
// vectors if any later step fails.
func (m *Manager) reembed(cand embedder.Embedder, ci embedder.ModelInfo, size int) func(context.Context) error {
	return func(ctx context.Context) error {
		if size > m.cfg.MaxReembedEntries {
			return fmt.Errorf("re-embedding %d entries exceeds the limit of %d; choose flush", size, m.cfg.MaxReembedEntries)
		}
		old, err := sortedEntries(m.slots.Store)
		if err != nil {
			return err
		}
		next := make([]persistence.Entry, len(old))
		var mu sync.Mutex
		var firstErr error
		sem := make(chan struct{}, 4)
		var wg sync.WaitGroup
		for i := range old {
			if strings.TrimSpace(old[i].Prompt) == "" {
				return fmt.Errorf("entry %s has no stored prompt, so it cannot be re-embedded; choose flush", old[i].ID)
			}
			wg.Add(1)
			sem <- struct{}{}
			go func(i int) {
				defer wg.Done()
				defer func() { <-sem }()
				ectx, cancel := context.WithTimeout(ctx, 30*time.Second)
				defer cancel()
				v, err := cand.Embed(ectx, old[i].Prompt)
				if err == nil && len(v) != ci.Dim {
					err = fmt.Errorf("vector has %d components, want %d", len(v), ci.Dim)
				}
				mu.Lock()
				defer mu.Unlock()
				if err != nil {
					if firstErr == nil {
						firstErr = fmt.Errorf("re-embedding entry %s: %w", old[i].ID, err)
					}
					return
				}
				e := old[i]
				e.Vector = v
				next[i] = e
			}(i)
		}
		wg.Wait()
		if firstErr != nil {
			return firstErr
		}

		toRebuild := func(es []persistence.Entry) []vectorstore.RebuildEntry {
			out := make([]vectorstore.RebuildEntry, len(es))
			for i, e := range es {
				out[i] = vectorstore.RebuildEntry{ID: e.ID, Vector: e.Vector}
			}
			return out
		}
		restore := func(cause error) error {
			var problems []string
			for _, e := range old {
				if err := m.slots.Store.Save(e); err != nil {
					problems = append(problems, err.Error())
					break
				}
			}
			if _, err := m.slots.VectorStore.Rebuild(ctx, toRebuild(old)); err != nil {
				problems = append(problems, "vector store restore: "+err.Error())
			}
			if len(problems) > 0 {
				return fmt.Errorf("%w; RESTORING THE PREVIOUS VECTORS ALSO FAILED (%s): flush the cache to recover", cause, strings.Join(problems, "; "))
			}
			return cause
		}
		for _, e := range next {
			if err := m.slots.Store.Save(e); err != nil {
				return restore(fmt.Errorf("saving re-embedded entry %s: %w", e.ID, err))
			}
		}
		if n, err := m.slots.VectorStore.Rebuild(ctx, toRebuild(next)); err != nil || n != len(next) {
			return restore(fmt.Errorf("rebuilding the vector store with new vectors: %v (restored %d of %d)", err, n, len(next)))
		}
		return nil
	}
}

func (m *Manager) planVectorStore(ctx context.Context, target *registry.Record, impl any, conf registry.Confirmations, host Host) (*plan, error) {
	cand, ok := impl.(vectorstore.VectorStore)
	if !ok {
		return nil, fmt.Errorf("candidate is not a vector store")
	}
	var ci vectorstore.Info
	if ip, ok := impl.(vectorstore.InfoProvider); ok {
		ci, _ = ip.Info(ctx)
	}
	if ei, err := m.slots.Embedder.ModelInfo(ctx); err == nil && ci.Dim > 0 && ei.Dim != ci.Dim {
		return nil, &IncompatibleError{fmt.Sprintf("the candidate vector store holds %d-dimensional vectors but the active embedder produces %d-dimensional vectors", ci.Dim, ei.Dim)}
	}
	var curInfo vectorstore.Info
	if ip, ok := m.slots.VectorStore.Current().(vectorstore.InfoProvider); ok {
		curInfo, _ = ip.Info(ctx)
	}

	pl := &plan{needGate: true}
	isMetric := target != nil && target.Type == plugins.TypeSimilarityMetric
	metricChanged := isMetric || (ci.Metric != "" && curInfo.Metric != "" && ci.Metric != curInfo.Metric)
	if metricChanged {
		if conf.Threshold == nil {
			return nil, &RequirementError{
				Field:   "threshold",
				Message: fmt.Sprintf("the similarity metric changes (%q -> %q); confirm the similarity threshold to use with it (currently %.4g)", orUnknown(curInfo.Metric), metricName(target, ci), host.Threshold()),
				Warning: "threshold scales differ between metrics: cosine distance is in [0, 2], Euclidean distance is unbounded and depends on vector length. A threshold tuned for one metric can cause wrong hits or no hits on another",
			}
		}
		t := *conf.Threshold
		if math.IsNaN(t) || math.IsInf(t, 0) || t < 0 {
			return nil, invalidf("threshold must be a finite number >= 0")
		}
		pl.threshold = &t
		pl.warnings = append(pl.warnings, fmt.Sprintf("similarity threshold set to %.4g for the new metric", t))
	} else if target != nil {
		pl.warnings = append(pl.warnings, fmt.Sprintf("the candidate does not report its metric; the threshold stays %.4g -- verify hit quality", host.Threshold()))
	}

	if conf.VectorCompat == "flush" {
		pl.migrate = func(ctx context.Context) error { return host.FlushCache(ctx) }
		pl.warnings = append(pl.warnings, "the cache was flushed at the administrator's request")
		return pl, nil
	}
	pl.migrate = func(ctx context.Context) error {
		entries, err := sortedEntries(m.slots.Store)
		if err != nil {
			return fmt.Errorf("list persisted entries: %w", err)
		}
		re := make([]vectorstore.RebuildEntry, len(entries))
		for i, e := range entries {
			if ci.Dim > 0 && len(e.Vector) != ci.Dim {
				return &RequirementError{
					Field: "vector_compat", Options: []string{"flush"},
					Message: fmt.Sprintf("%d persisted vectors have dimension %d but the candidate expects %d; the cache must be flushed", len(entries), len(e.Vector), ci.Dim),
					Warning: "flush permanently deletes all cached replies",
				}
			}
			re[i] = vectorstore.RebuildEntry{ID: e.ID, Vector: e.Vector}
		}
		n, err := cand.Rebuild(ctx, re)
		if err != nil {
			return fmt.Errorf("rebuild candidate from persistence: %w", err)
		}
		if n != len(re) {
			return fmt.Errorf("candidate restored %d of %d entries", n, len(re))
		}
		size, err := cand.Size(ctx)
		if err != nil || size != len(re) {
			return fmt.Errorf("candidate reports size %d after rebuild (%v), want %d", size, err, len(re))
		}
		// Behavioural check: a stored vector must find itself.
		for _, i := range samplePositions(len(re)) {
			ms, err := cand.Search(ctx, re[i].Vector, minInt(3, len(re)), 1e9)
			if err != nil {
				return fmt.Errorf("verify search on candidate: %w", err)
			}
			found := false
			for _, mt := range ms {
				found = found || mt.ID == re[i].ID
			}
			if !found {
				return fmt.Errorf("verify search: candidate did not return stored entry %s for its own vector", re[i].ID)
			}
		}
		return nil
	}
	pl.cleanup = func() {
		cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = cand.Flush(cctx)
	}
	return pl, nil
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

func metricName(target *registry.Record, ci vectorstore.Info) string {
	if target != nil && target.Manifest != nil && target.Manifest.Spec.Python != nil && target.Type == plugins.TypeSimilarityMetric {
		return target.Manifest.Spec.Python.Backend
	}
	return orUnknown(ci.Metric)
}

func samplePositions(n int) []int {
	switch {
	case n == 0:
		return nil
	case n == 1:
		return []int{0}
	case n == 2:
		return []int{0, 1}
	}
	return []int{0, n / 2, n - 1}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// rebuildFrom makes the vector store and eviction policy describe the entries of
// s exactly (used when a different store becomes the source of truth).
func (m *Manager) rebuildFrom(ctx context.Context, s persistence.Store) (int, error) {
	entries, err := sortedEntries(s)
	if err != nil {
		return 0, fmt.Errorf("list entries of the new store: %w", err)
	}
	re := make([]vectorstore.RebuildEntry, len(entries))
	for i, e := range entries {
		re[i] = vectorstore.RebuildEntry{ID: e.ID, Vector: e.Vector}
	}
	n, err := m.slots.VectorStore.Rebuild(ctx, re)
	if err != nil {
		return 0, fmt.Errorf("rebuild the vector store from the new store: %w", err)
	}
	if n != len(re) {
		return 0, fmt.Errorf("vector store restored %d of %d entries", n, len(re))
	}
	m.slots.Policy.Flush()
	for _, e := range entries {
		m.slots.Policy.OnInsert(e.ID)
	}
	return len(entries), nil
}

func (m *Manager) planPersistence(ctx context.Context, impl any, conf registry.Confirmations, host Host) (*plan, error) {
	cand, ok := impl.(persistence.Store)
	if !ok {
		return nil, fmt.Errorf("candidate is not a persistence store")
	}
	if cand == m.slots.Store.Current() {
		return nil, invalidf("candidate is already the active store")
	}
	entries, err := sortedEntries(m.slots.Store)
	if err != nil {
		return nil, fmt.Errorf("read the current cache entries: %w", err)
	}
	candSize, err := cand.Size()
	if err != nil {
		return nil, fmt.Errorf("read the candidate store: %w", err)
	}
	pl := &plan{needGate: true}

	// adopt: the candidate's own contents become the cache.
	adopt := func(reason string) {
		pl.after = func(ctx context.Context) error { _, err := m.rebuildFrom(ctx, cand); return err }
		pl.warnings = append(pl.warnings, reason)
	}
	if len(entries) == 0 {
		if candSize > 0 {
			adopt(fmt.Sprintf("the new store already holds %d entries; the vector index and eviction policy were rebuilt from it", candSize))
		}
		return pl, nil
	}

	switch conf.PersistenceCompat {
	case "migrate":
		if candSize != 0 {
			return nil, &IncompatibleError{fmt.Sprintf("the candidate store must be empty to migrate into it (it holds %d entries); choose adopt to use its contents instead", candSize)}
		}
		var saved []string
		pl.migrate = func(ctx context.Context) error {
			// Writes are paused, so this is a consistent snapshot.
			entries, err := sortedEntries(m.slots.Store)
			if err != nil {
				return err
			}
			for _, e := range entries {
				if err := cand.Save(e); err != nil {
					return fmt.Errorf("copy entry %s to the candidate: %w", e.ID, err)
				}
				saved = append(saved, e.ID)
			}
			n, err := cand.Size()
			if err != nil || n != len(entries) {
				return fmt.Errorf("migration count mismatch: candidate holds %d entries (err=%v), expected %d", n, err, len(entries))
			}
			for _, e := range entries {
				var got persistence.Entry
				if dl, ok := cand.(persistence.DetailedLoader); ok {
					got, err = dl.LoadDetailed(e.ID)
				} else {
					var found bool
					got, found = cand.Load(e.ID)
					if !found {
						err = persistence.ErrNotFound
					}
				}
				if err != nil {
					return fmt.Errorf("verify entry %s in the candidate: %w", e.ID, err)
				}
				if got.Reply != e.Reply || got.Prompt != e.Prompt || len(got.Vector) != len(e.Vector) || !got.CreatedAt.Equal(e.CreatedAt) {
					return fmt.Errorf("verify entry %s in the candidate: stored data differs from the source", e.ID)
				}
			}
			return nil
		}
		pl.cleanup = func() {
			for _, id := range saved {
				_ = cand.Delete(id)
			}
		}
		pl.warnings = append(pl.warnings, fmt.Sprintf("migrated %d cache entries to the new store; the old store's data is left untouched for rollback", len(entries)))
	case "empty":
		if candSize != 0 {
			return nil, &IncompatibleError{fmt.Sprintf("the candidate store holds %d entries, so it does not start empty; choose adopt to use them", candSize)}
		}
		pl.after = func(ctx context.Context) error {
			// The new store is empty: vectors and policy state for the old
			// entries would point at nothing. The old store itself is NOT flushed.
			if err := m.slots.VectorStore.Flush(ctx); err != nil {
				return fmt.Errorf("flush vector store: %w", err)
			}
			m.slots.Policy.Flush()
			return nil
		}
		pl.warnings = append(pl.warnings, fmt.Sprintf("started with an empty cache; %d entries remain in the previous store (not deleted) and are not served", len(entries)))
	case "adopt":
		adopt(fmt.Sprintf("switched to the candidate's existing %d entries; the %d entries of the previous store (left intact) are no longer served", candSize, len(entries)))
	default:
		opts := []string{"adopt"}
		msg := fmt.Sprintf("the current cache holds %d entries and the candidate store holds %d. Choose adopt to serve the candidate's own contents (the current entries stay in the old store)", len(entries), candSize)
		if candSize == 0 {
			opts = []string{"migrate", "empty"}
			msg = fmt.Sprintf("the current cache holds %d entries. Choose migrate (copy them to the new store and verify the count) or empty (start with an empty cache; the old entries are kept in the old store but not served)", len(entries))
		}
		return nil, &RequirementError{
			Field: "persistence_compat", Options: opts, Message: msg,
			Warning: "cached replies stop being served (empty/adopt) or are copied (migrate); nothing is deleted from the old store",
		}
	}
	return pl, nil
}
