// Package registry persists plugin records, active selections, audit events and
// sanitized logs. It uses its own Redis key namespace (llm_cache_plugins:), kept
// separate from the semantic-cache entries (llm_cache:).
package registry

import (
	"errors"
	"time"

	"github.com/leenkabha/llm_cache/internal/plugins"
	"github.com/leenkabha/llm_cache/internal/plugins/manifest"
	"github.com/leenkabha/llm_cache/internal/plugins/secrets"
)

var (
	ErrNotFound = errors.New("plugin not found")
	ErrExists   = errors.New("plugin already exists")
	ErrConflict = errors.New("plugin was modified concurrently")
)

// Source is the safe (non-secret) description of where a plugin came from.
type Source struct {
	EndpointURL string `json:"endpoint_url,omitempty"` // no userinfo, query or fragment
	ImageRef    string `json:"image_ref,omitempty"`
	RepoURL     string `json:"repo_url,omitempty"`
	Revision    string `json:"revision,omitempty"` // requested revision
}

// Health is the last observed health of the plugin.
type Health struct {
	Status    string    `json:"status"` // unknown | healthy | unhealthy
	Message   string    `json:"message,omitempty"`
	CheckedAt time.Time `json:"checked_at,omitempty"`
}

// Check is one contract-test result.
type Check struct {
	Name   string `json:"name"`
	Status string `json:"status"` // pass | fail | skip
	Detail string `json:"detail,omitempty"`
}

// Verification is the outcome of the most recent contract run.
type Verification struct {
	At      time.Time `json:"at"`
	Passed  bool      `json:"passed"`
	Checks  []Check   `json:"checks"`
	Summary string    `json:"summary,omitempty"`
}

// ScanReport summarises image scanning. Status "skipped" is recorded honestly
// when no scanner is configured.
type ScanReport struct {
	Scanner   string         `json:"scanner"`
	Status    string         `json:"status"` // passed | failed | skipped
	Counts    map[string]int `json:"counts,omitempty"`
	Message   string         `json:"message,omitempty"`
	ScannedAt time.Time      `json:"scanned_at,omitempty"`
}

// Provenance records how an image came to be.
type Provenance struct {
	Builder     string    `json:"builder,omitempty"`
	RepoURL     string    `json:"repo_url,omitempty"`
	Commit      string    `json:"commit,omitempty"`
	SourceHash  string    `json:"source_sha256,omitempty"`
	ManifestSHA string    `json:"manifest_sha256,omitempty"`
	BuiltAt     time.Time `json:"built_at,omitempty"`
	ImageSize   int64     `json:"image_size_bytes,omitempty"`
}

// Confirmations are the explicit administrator decisions activation may need.
type Confirmations struct {
	// Embedder / embedding-model: "flush" empties the cache, "reembed" recomputes
	// every stored vector with the new model. Empty means none given.
	EmbedderCompat string `json:"embedder_compat,omitempty"`
	// Similarity metric: the threshold to use with the new metric.
	Threshold *float64 `json:"threshold,omitempty"`
	// Persistence: "migrate" copies entries, "empty" starts with an empty cache.
	PersistenceCompat string `json:"persistence_compat,omitempty"`
	// Vector store / index / metric: "rebuild" from persistence (default) or "flush".
	VectorCompat string `json:"vector_compat,omitempty"`
}

// Record is the complete stored state of one plugin installation.
type Record struct {
	ID              string             `json:"id"`
	Name            string             `json:"name"`
	Version         string             `json:"version"`
	Type            plugins.Type       `json:"type"`
	ContractVersion string             `json:"contract_version"`
	Mode            plugins.Mode       `json:"mode"`
	Source          Source             `json:"source"`
	Commit          string             `json:"commit,omitempty"`
	ImageDigest     string             `json:"image_digest,omitempty"`
	Manifest        *manifest.Manifest `json:"manifest,omitempty"`
	Config          map[string]any     `json:"config,omitempty"`
	// Secrets holds encrypted blobs only. It is never copied into an API view.
	Secrets          map[string]secrets.Sealed `json:"secrets,omitempty"`
	State            plugins.State             `json:"state"`
	StateDetail      string                    `json:"state_detail,omitempty"`
	Health           Health                    `json:"health"`
	Active           bool                      `json:"active"`
	PreviousActiveID string                    `json:"previous_active_id,omitempty"` // what to roll back to
	// PreviousThreshold is the similarity threshold in force before this plugin
	// changed it (similarity metric or vector store with a different metric), so
	// a rollback or deactivation can restore it.
	PreviousThreshold *float64      `json:"previous_threshold,omitempty"`
	Confirmations     Confirmations `json:"confirmations"`
	Verification      *Verification `json:"verification,omitempty"`
	Scan              *ScanReport   `json:"scan,omitempty"`
	Provenance        *Provenance   `json:"provenance,omitempty"`
	InstanceID        string        `json:"instance_id,omitempty"` // controller instance running this plugin
	// UpgradedFromID links an upgrade candidate to the record it would replace.
	UpgradedFromID string    `json:"upgraded_from_id,omitempty"`
	Rev            int64     `json:"rev"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// SecretNames lists stored secret names (never values).
func (r *Record) SecretNames() []string {
	names := make([]string, 0, len(r.Secrets))
	for n := range r.Secrets {
		names = append(names, n)
	}
	return names
}

// Clone returns a deep-enough copy for safe mutation by callers.
func (r *Record) Clone() *Record {
	c := *r
	if r.Config != nil {
		c.Config = make(map[string]any, len(r.Config))
		for k, v := range r.Config {
			c.Config[k] = v
		}
	}
	if r.Secrets != nil {
		c.Secrets = make(map[string]secrets.Sealed, len(r.Secrets))
		for k, v := range r.Secrets {
			c.Secrets[k] = v
		}
	}
	if r.Verification != nil {
		v := *r.Verification
		v.Checks = append([]Check(nil), r.Verification.Checks...)
		c.Verification = &v
	}
	if r.Scan != nil {
		s := *r.Scan
		c.Scan = &s
	}
	if r.Provenance != nil {
		p := *r.Provenance
		c.Provenance = &p
	}
	if r.Confirmations.Threshold != nil {
		t := *r.Confirmations.Threshold
		c.Confirmations.Threshold = &t
	}
	if r.PreviousThreshold != nil {
		t := *r.PreviousThreshold
		c.PreviousThreshold = &t
	}
	return &c
}

// Event is one audit entry. Detail is always scrubbed before storage.
type Event struct {
	Time     time.Time    `json:"time"`
	Actor    string       `json:"actor"`
	Action   string       `json:"action"`
	PluginID string       `json:"plugin_id,omitempty"`
	Type     plugins.Type `json:"type,omitempty"`
	Outcome  string       `json:"outcome"` // ok | failed | denied
	Detail   string       `json:"detail,omitempty"`
}

// Store persists records and related data.
type Store interface {
	Get(id string) (*Record, error)
	List() ([]*Record, error)
	Create(r *Record) error
	// Update applies fn atomically to the stored record and returns the result.
	// If fn returns an error nothing is written.
	Update(id string, fn func(*Record) error) (*Record, error)
	Delete(id string) error

	// Active maps a slot to the ID of its active plugin.
	Active() (map[plugins.Type]string, error)
	SetActive(slot plugins.Type, id string) error
	ClearActive(slot plugins.Type) error

	AppendAudit(e Event) error
	Audit(pluginID string, limit int) ([]Event, error)
	AppendLog(id, line string) error
	Logs(id string, limit int) ([]string, error)
	Ping() error
}

const (
	maxAuditEvents = 1000
	maxLogLines    = 500
	maxLogLineLen  = 2000
)
