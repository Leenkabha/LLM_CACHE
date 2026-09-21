package manager

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/leenkabha/llm_cache/internal/plugins"
	"github.com/leenkabha/llm_cache/internal/plugins/contract"
	"github.com/leenkabha/llm_cache/internal/plugins/manifest"
	"github.com/leenkabha/llm_cache/internal/plugins/protocol"
	"github.com/leenkabha/llm_cache/internal/plugins/registry"
	"github.com/leenkabha/llm_cache/internal/plugins/safehttp"
	"github.com/leenkabha/llm_cache/internal/plugins/secrets"
)

// Manager is the plugin control plane.
type Manager struct {
	cfg   Config
	reg   registry.Store
	box   *secrets.Box
	slots *Slots
	ctl   Controller
	ctlHC *http.Client // reaches the controller proxy; the controller URL is trusted operator config

	hostMu sync.RWMutex
	host   Host

	slotMu            map[plugins.Type]*sync.Mutex // serialises activation per slot
	stateMu           sync.Mutex
	active            map[plugins.Type]string // slot -> active record id ("" = built-in)
	owned             map[string]any          // record id -> adapter this manager built for it
	timers            map[string]*time.Timer  // record id -> pending "stop old instance" timer
	restoredThreshold *float64
	builtinNames      map[plugins.Type]string

	wg sync.WaitGroup
}

// New builds a Manager. box may be nil only when cfg.Enabled is false.
func New(cfg Config, reg registry.Store, box *secrets.Box, slots *Slots, ctl Controller) *Manager {
	cfg.Defaults()
	m := &Manager{
		cfg: cfg, reg: reg, box: box, slots: slots, ctl: ctl,
		slotMu: map[plugins.Type]*sync.Mutex{},
		active: map[plugins.Type]string{}, owned: map[string]any{}, timers: map[string]*time.Timer{},
	}
	for _, s := range slotTypes {
		m.slotMu[s] = &sync.Mutex{}
	}
	m.ctlHC = &http.Client{Timeout: 5 * time.Minute, Transport: &http.Transport{Proxy: nil, MaxIdleConnsPerHost: 16}}
	return m
}

// SetHost attaches the running orchestrator.
func (m *Manager) SetHost(h Host) {
	m.hostMu.Lock()
	m.host = h
	m.hostMu.Unlock()
}

func (m *Manager) getHost() (Host, error) {
	m.hostMu.RLock()
	defer m.hostMu.RUnlock()
	if m.host == nil {
		return nil, ErrNoHost
	}
	return m.host, nil
}

// SetBuiltinNames records a human-readable name for each built-in
// implementation (for example "gemini" or "redis") shown next to the plugin
// that replaces it.
func (m *Manager) SetBuiltinNames(n map[plugins.Type]string) { m.builtinNames = n }

// Component describes one pluggable component and what currently serves it.
type Component struct {
	Type       plugins.Type `json:"type"`
	Slot       plugins.Type `json:"slot"`
	Python     bool         `json:"python"`
	Builtin    string       `json:"builtin"`
	ActiveID   string       `json:"active_id,omitempty"`
	ActiveName string       `json:"active_name,omitempty"`
	ActiveVer  string       `json:"active_version,omitempty"`
}

// Components lists all nine plugin types with their active implementation.
func (m *Manager) Components() []Component {
	active := m.ActiveIDs()
	out := make([]Component, 0, len(plugins.AllTypes))
	for _, t := range plugins.AllTypes {
		c := Component{Type: t, Slot: t.Slot(), Python: t.IsPython(), Builtin: m.builtinNames[t.Slot()]}
		if t.IsPython() {
			c.Builtin = m.builtinNames[t]
		}
		if id := active[t.Slot()]; id != "" {
			if rec, err := m.reg.Get(id); err == nil && rec.Type == t {
				c.ActiveID, c.ActiveName, c.ActiveVer = rec.ID, rec.Name, rec.Version
			}
		}
		out = append(out, c)
	}
	return out
}

// Enabled reports whether management is on.
func (m *Manager) Enabled() bool { return m.cfg.Enabled }

// Config returns the effective configuration (no secrets except none are held).
func (m *Manager) Settings() map[string]any {
	return map[string]any{
		"installation_enabled":     m.cfg.Enabled,
		"controller_configured":    m.ctl != nil,
		"allow_insecure_endpoints": m.cfg.AllowInsecureEndpoints,
		"rollback_window_seconds":  int(m.cfg.RollbackWindow / time.Second),
	}
}

func (m *Manager) policy() safehttp.Policy {
	return safehttp.Policy{AllowInsecure: m.cfg.AllowInsecureEndpoints}
}

func (m *Manager) requireEnabled() error {
	if !m.cfg.Enabled || m.box == nil {
		return ErrDisabled
	}
	return nil
}

// WaitIdle blocks until background pipelines have finished (used by tests and shutdown).
func (m *Manager) WaitIdle() { m.wg.Wait() }

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "plg_" + hex.EncodeToString(b)
}

// ---- audit and logging -------------------------------------------------------

func (m *Manager) audit(action string, rec *registry.Record, outcome, detail string) {
	e := registry.Event{Time: time.Now().UTC(), Actor: "admin", Action: action, Outcome: outcome, Detail: secrets.Scrub(detail)}
	if rec != nil {
		e.PluginID, e.Type = rec.ID, rec.Type
	}
	if err := m.reg.AppendAudit(e); err != nil {
		log.Printf("plugin audit: %v", err)
	}
}

// logf appends a sanitised line to the plugin's log and to the process log.
func (m *Manager) logf(id, format string, a ...any) {
	line := secrets.Scrub(fmt.Sprintf(format, a...))
	if err := m.reg.AppendLog(id, time.Now().UTC().Format("15:04:05")+" "+line); err != nil {
		log.Printf("plugin log: %v", err)
	}
	log.Printf("plugin %s: %s", id, line)
}

// ---- records -----------------------------------------------------------------

func (m *Manager) setState(id string, to plugins.State, detail string) (*registry.Record, error) {
	return m.reg.Update(id, func(r *registry.Record) error {
		if !plugins.ValidTransition(r.State, to) {
			return fmt.Errorf("illegal lifecycle transition %s -> %s", r.State, to)
		}
		r.State, r.StateDetail = to, secrets.Scrub(detail)
		return nil
	})
}

// Get returns one record.
func (m *Manager) Get(id string) (*registry.Record, error) { return m.reg.Get(id) }

// List returns all records.
func (m *Manager) List() ([]*registry.Record, error) { return m.reg.List() }

// ActiveIDs returns the active record ID for every slot ("" = built-in).
func (m *Manager) ActiveIDs() map[plugins.Type]string {
	m.stateMu.Lock()
	defer m.stateMu.Unlock()
	out := make(map[plugins.Type]string, len(m.active))
	for k, v := range m.active {
		out[k] = v
	}
	return out
}

// Logs returns the sanitised log lines for a plugin.
func (m *Manager) Logs(id string, limit int) ([]string, error) {
	if err := m.requireEnabled(); err != nil {
		return nil, err
	}
	if _, err := m.reg.Get(id); err != nil {
		return nil, err
	}
	return m.reg.Logs(id, limit)
}

// Audit returns recent audit events.
func (m *Manager) Audit(pluginID string, limit int) ([]registry.Event, error) {
	return m.reg.Audit(pluginID, limit)
}

// openSecrets decrypts a record's secrets. The result is for immediate use only.
func (m *Manager) openSecrets(rec *registry.Record) (map[string]string, error) {
	out := make(map[string]string, len(rec.Secrets))
	for name, sealed := range rec.Secrets {
		v, err := m.box.Open(rec.ID, name, sealed)
		if err != nil {
			return nil, fmt.Errorf("secret %s: %w", name, err)
		}
		out[name] = v
	}
	return out, nil
}

func secretValues(s map[string]string) []string {
	out := make([]string, 0, len(s))
	for _, v := range s {
		out = append(out, v)
	}
	return out
}

// ---- request validation ------------------------------------------------------

var (
	imageRE    = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*(:[0-9]+)?(/[a-z0-9][a-z0-9._-]*)*(:[A-Za-z0-9_][A-Za-z0-9._-]{0,127})?(@sha256:[a-f0-9]{64})?$`)
	revisionRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,99}$`)
	// Development only (ALLOW_INSECURE_PLUGIN_ENDPOINTS): an absolute path to a local
	// git repository, which the controller also refuses unless PLUGIN_ALLOW_LOCAL_GIT.
	localPathRE = regexp.MustCompile(`^/[A-Za-z0-9._/-]{1,200}$`)
	repoRE      = regexp.MustCompile(`^https://github\.com/[A-Za-z0-9][A-Za-z0-9-]{0,38}/[A-Za-z0-9._-]{1,100}?(\.git)?$`)
)

const maxSecretLen = 8 << 10

// parsedRequest is a validated install/verify request.
type parsedRequest struct {
	typ      plugins.Type
	mode     plugins.Mode
	manifest *manifest.Manifest // nil until the controller supplies it (repository/image)
	source   registry.Source
	config   map[string]any
	secrets  map[string]string
}

func (m *Manager) parseRequest(req InstallRequest) (*parsedRequest, error) {
	typ, err := plugins.ParseType(req.Type)
	if err != nil {
		return nil, invalidf("%v", err)
	}
	p := &parsedRequest{typ: typ, config: req.Config, secrets: req.Secrets}

	for name, v := range req.Secrets {
		if !secretNameRE.MatchString(name) {
			return nil, invalidf("secret name %q is not valid", name)
		}
		if v == "" {
			return nil, invalidf("secret %s is empty", name)
		}
		if len(v) > maxSecretLen {
			return nil, invalidf("secret %s is too long", name)
		}
	}

	if req.Manifest != "" {
		mf, err := manifest.Parse([]byte(req.Manifest))
		if err != nil {
			return nil, &InvalidError{err.Error()}
		}
		if mf.Spec.Type != string(typ) {
			return nil, invalidf("manifest is for type %s but the request selects %s", mf.Spec.Type, typ)
		}
		p.manifest = mf
	}

	switch req.Mode {
	case string(plugins.ModeEndpoint):
		p.mode = plugins.ModeEndpoint
		u, err := safehttp.ValidateURL(req.Endpoint, m.policy())
		if err != nil {
			return nil, &InvalidError{"endpoint: " + err.Error()}
		}
		p.source = registry.Source{EndpointURL: u.String()}
		if p.manifest == nil {
			name, version := req.Name, req.Version
			if version == "" {
				version = "1.0.0"
			}
			mf := manifest.Minimal(name, version, typ)
			if err := mf.Validate(); err != nil {
				return nil, &InvalidError{err.Error()}
			}
			p.manifest = mf
		}
		for n := range req.Secrets {
			if n != EndpointTokenSecret {
				return nil, invalidf("a hosted endpoint only accepts the %s secret (sent as a Bearer token); %q would never be used", EndpointTokenSecret, n)
			}
		}
	case string(plugins.ModeImage):
		p.mode = plugins.ModeImage
		if m.ctl == nil {
			return nil, invalidf("image installation needs the plugin controller (PLUGIN_CONTROLLER_URL is not set)")
		}
		if !imageRE.MatchString(req.Image) || strings.Contains(req.Image, "..") {
			return nil, invalidf("image reference %q is not valid", req.Image)
		}
		p.source = registry.Source{ImageRef: req.Image}
	case string(plugins.ModeRepository):
		p.mode = plugins.ModeRepository
		if m.ctl == nil {
			return nil, invalidf("repository installation needs the plugin controller (PLUGIN_CONTROLLER_URL is not set)")
		}
		localOK := m.cfg.AllowInsecureEndpoints && filepath.IsAbs(req.Repo) && localPathRE.MatchString(req.Repo)
		if !repoRE.MatchString(req.Repo) && !localOK {
			return nil, invalidf("repository must be an https://github.com/<owner>/<repo> URL")
		}
		rev := req.Revision
		if rev == "" {
			rev = "HEAD"
		}
		if !revisionRE.MatchString(rev) || strings.Contains(rev, "..") {
			return nil, invalidf("revision %q is not valid", rev)
		}
		p.source = registry.Source{RepoURL: req.Repo, Revision: rev}
	default:
		return nil, invalidf("mode must be endpoint, image or repository")
	}
	if p.manifest != nil {
		if p.mode != plugins.ModeEndpoint && p.manifest.Spec.Runtime.Mode == manifest.RuntimeEndpoint {
			return nil, invalidf("manifest runtime.mode is endpoint but the install mode is %s", p.mode)
		}
		if err := m.checkConfigAndSecrets(p.manifest, p.mode, req.Config, req.Secrets); err != nil {
			return nil, err
		}
	}
	return p, nil
}

var secretNameRE = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)

func (m *Manager) checkConfigAndSecrets(mf *manifest.Manifest, mode plugins.Mode, cfg map[string]any, sec map[string]string) error {
	if _, err := mf.ResolveConfig(cfg); err != nil {
		return &InvalidError{err.Error()}
	}
	if mode == plugins.ModeEndpoint {
		return nil // endpoints take only ENDPOINT_TOKEN, checked above
	}
	names := make([]string, 0, len(sec))
	for n := range sec {
		names = append(names, n)
	}
	if err := mf.CheckSecrets(names); err != nil {
		return &InvalidError{err.Error()}
	}
	return nil
}

// ---- adapters ----------------------------------------------------------------

func configEnv(cfg map[string]any) map[string]string {
	env := map[string]string{}
	for k, v := range cfg {
		env["CONFIG_"+strings.ToUpper(k)] = fmt.Sprint(v)
	}
	return env
}

// ensureInstance starts (or re-uses) the container for a record.
func (m *Manager) ensureInstance(ctx context.Context, rec *registry.Record, sec map[string]string) (*Instance, error) {
	if m.ctl == nil {
		return nil, fmt.Errorf("plugin controller is not configured")
	}
	if rec.ImageDigest == "" || rec.Manifest == nil {
		return nil, fmt.Errorf("plugin has no prepared image")
	}
	mtext, err := yaml.Marshal(rec.Manifest)
	if err != nil {
		return nil, err
	}
	env := configEnv(rec.Config)
	for k, v := range sec {
		env[k] = v
	}
	if rec.Type == plugins.TypeVectorIndex || rec.Type == plugins.TypeSimilarityMetric {
		// The vector runner must be sized for the embedder that is (or will be) in
		// use; the built-in FAISS service defaults to the same 384.
		dim := 384
		ictx, icancel := context.WithTimeout(ctx, 5*time.Second)
		if mi, err := m.slots.Embedder.ModelInfo(ictx); err == nil && mi.Dim > 0 {
			dim = mi.Dim
		}
		icancel()
		env["VECTOR_DIM"] = fmt.Sprint(dim)
	}
	inst, err := m.ctl.Start(ctx, StartRequest{
		InstanceID: rec.ID, Type: rec.Type, Digest: rec.ImageDigest, Manifest: string(mtext),
		Env: env, HealthWait: m.cfg.HealthTimeout,
	})
	if err != nil {
		return nil, fmt.Errorf("start plugin: %s", secrets.Scrub(err.Error(), secretValues(sec)...))
	}
	return inst, nil
}

func (m *Manager) clientFor(ctx context.Context, rec *registry.Record, sec map[string]string) (*protocol.Client, error) {
	switch rec.Mode {
	case plugins.ModeEndpoint:
		u, err := safehttp.ValidateURL(rec.Source.EndpointURL, m.policy())
		if err != nil {
			return nil, err
		}
		return protocol.NewClient(u.String(), safehttp.NewClient(m.policy(), 60*time.Second), sec[EndpointTokenSecret])
	default:
		inst, err := m.ensureInstance(ctx, rec, sec)
		if err != nil {
			return nil, err
		}
		return protocol.NewClient(inst.BaseURL, m.ctlHC, m.cfg.ControllerToken)
	}
}

func modelOf(rec *registry.Record) string {
	if s, ok := rec.Config["model"].(string); ok && s != "" {
		return s
	}
	return "default"
}

// buildAdapter creates the Go-side adapter for a record. The returned value is
// one of llm.Backend, embedder.Embedder, vectorstore.VectorStore,
// persistence.Store, cachequeue.Queue or policy.EvictionPolicy.
func (m *Manager) buildAdapter(ctx context.Context, rec *registry.Record, sec map[string]string) (any, error) {
	c, err := m.clientFor(ctx, rec, sec)
	if err != nil {
		return nil, err
	}
	if s, ok := rec.Config["request_timeout"].(string); ok {
		if d, err := time.ParseDuration(s); err == nil && d > 0 {
			c.Timeout = d
		}
	}
	switch rec.Type {
	case plugins.TypeLLM:
		if c.Timeout == protocol.DefaultTimeout {
			c.Timeout = 60 * time.Second
		}
		return protocol.NewLLM(c, modelOf(rec)), nil
	case plugins.TypeEmbedder, plugins.TypeEmbeddingModel:
		return protocol.NewEmbedder(c), nil
	case plugins.TypeVectorStore, plugins.TypeVectorIndex, plugins.TypeSimilarityMetric:
		return protocol.NewVectorStore(c), nil
	case plugins.TypePersistence:
		return protocol.NewPersistence(c), nil
	case plugins.TypeQueue:
		return protocol.NewQueue(c, protocol.QueueOptions{}), nil
	case plugins.TypePolicy:
		return protocol.NewPolicy(ctx, c)
	}
	return nil, fmt.Errorf("no adapter for type %s", rec.Type)
}

// runContract executes the type's contract suite against the record's plugin.
func (m *Manager) runContract(ctx context.Context, rec *registry.Record, sec map[string]string) (*registry.Verification, error) {
	c, err := m.clientFor(ctx, rec, sec)
	if err != nil {
		return nil, err
	}
	t := contract.Target{Type: rec.Type, Client: c, Model: modelOf(rec), RequireEmpty: true}
	if rec.Manifest != nil {
		t.VectorDim = rec.Manifest.Spec.Verify.VectorDim
		if py := rec.Manifest.Spec.Python; py != nil {
			t.ExpectedBackend = py.Backend
		}
	}
	rep := contract.Run(ctx, t)
	v := &registry.Verification{At: time.Now().UTC(), Passed: rep.Passed(), Summary: rep.Summary()}
	for _, r := range rep.Results {
		v.Checks = append(v.Checks, registry.Check{Name: r.Name, Status: r.Status, Detail: r.Detail})
	}
	return v, nil
}

// ---- Verify ------------------------------------------------------------------

// VerifyResult is the outcome of a dry-run verification.
type VerifyResult struct {
	Valid        bool                         `json:"valid"`
	Name         string                       `json:"name,omitempty"`
	Version      string                       `json:"version,omitempty"`
	Type         plugins.Type                 `json:"type,omitempty"`
	Description  string                       `json:"description,omitempty"`
	ConfigSchema map[string]manifest.Property `json:"config_schema,omitempty"`
	Secrets      []manifest.SecretRef         `json:"secrets,omitempty"`
	Commit       string                       `json:"commit,omitempty"`
	Issues       []manifest.Issue             `json:"issues,omitempty"`
	Verification *registry.Verification       `json:"verification,omitempty"`
	Notes        []string                     `json:"notes,omitempty"`
}

// Verify validates a request without installing it. For a hosted endpoint it
// also runs the contract suite (against a candidate that must be empty). For a
// repository or image it resolves the manifest so the caller can render the
// configuration form; the full build and contract run happen at install.
// Nothing is persisted and secrets are held only in memory for the call.
func (m *Manager) Verify(ctx context.Context, req InstallRequest) (*VerifyResult, error) {
	if err := m.requireEnabled(); err != nil {
		return nil, err
	}
	res := &VerifyResult{}
	p, err := m.parseRequest(req)
	if err != nil {
		var ie *InvalidError
		if asInvalid(err, &ie) {
			res.Issues = []manifest.Issue{{Message: ie.Msg}}
			m.audit("verify", nil, "failed", ie.Msg)
			return res, nil
		}
		return nil, err
	}
	mf := p.manifest
	if mf == nil {
		// Repository/image without a supplied manifest: ask the controller.
		pr, err := m.ctl.Prepare(ctx, PrepareRequest{Mode: p.mode, Type: p.typ, Image: p.source.ImageRef, Repo: p.source.RepoURL, Revision: p.source.Revision, DryRun: true})
		if err != nil {
			res.Issues = []manifest.Issue{{Message: secrets.Scrub(err.Error())}}
			return res, nil
		}
		mf, err = manifest.Parse([]byte(pr.Manifest))
		if err != nil {
			res.Issues = []manifest.Issue{{Message: err.Error()}}
			return res, nil
		}
		if mf.Spec.Type != string(p.typ) {
			res.Issues = []manifest.Issue{{Path: "spec.type", Message: fmt.Sprintf("manifest is for type %s, not %s", mf.Spec.Type, p.typ)}}
			return res, nil
		}
		res.Commit = pr.Commit
		if err := m.checkConfigAndSecrets(mf, p.mode, req.Config, req.Secrets); err != nil {
			res.Issues = []manifest.Issue{{Message: err.Error()}}
			// still return the manifest so the form can be shown
		}
	}
	res.Name, res.Version, res.Type = mf.Metadata.Name, mf.Metadata.Version, p.typ
	res.Description, res.ConfigSchema, res.Secrets = mf.Metadata.Description, mf.Spec.Config.Properties, mf.Spec.Secrets
	res.Valid = len(res.Issues) == 0

	if p.mode == plugins.ModeEndpoint && res.Valid {
		rec := &registry.Record{ID: "verify", Type: p.typ, Mode: p.mode, Source: p.source, Manifest: mf, Config: p.config}
		v, err := m.runContract(ctx, rec, p.secrets)
		if err != nil {
			res.Valid = false
			res.Issues = append(res.Issues, manifest.Issue{Message: secrets.Scrub(err.Error(), secretValues(p.secrets)...)})
		} else {
			res.Verification = v
			res.Valid = v.Passed
		}
	} else if p.mode != plugins.ModeEndpoint {
		res.Notes = append(res.Notes, "the image or repository is built, scanned and contract-tested when you install it")
	}
	m.audit("verify", nil, map[bool]string{true: "ok", false: "failed"}[res.Valid], string(p.mode)+" "+string(p.typ))
	return res, nil
}

func marshalManifest(mf *manifest.Manifest) string {
	b, err := yaml.Marshal(mf)
	if err != nil {
		return ""
	}
	return string(b)
}

// closeAdapter releases a discarded adapter's resources.
func closeAdapter(a any) {
	if c, ok := a.(interface{ Close() }); ok {
		c.Close()
	}
}
