// Package manager is the plugin control plane. It owns the plugin registry,
// validates manifests, coordinates the plugin controller, runs contract tests,
// and performs component-specific activation with rollback.
//
// Invariants it maintains:
//   - a plugin becomes active only after its contract tests and health check
//     pass and any state it depends on has been migrated, rebuilt or replayed;
//   - the previous implementation stays in place until the atomic switch, so a
//     failed verification, migration or activation leaves it untouched;
//   - state is never flushed or mixed silently: anything that would discard or
//     invalidate cache data needs an explicit administrator confirmation;
//   - secrets exist in plaintext only in memory, briefly, when a plugin starts.
package manager

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/leenkabha/llm_cache/internal/plugins"
	"github.com/leenkabha/llm_cache/internal/plugins/ctlapi"
	"github.com/leenkabha/llm_cache/internal/plugins/manifest"
	"github.com/leenkabha/llm_cache/internal/plugins/registry"
)

// Config is the manager's operator-facing configuration.
type Config struct {
	// Enabled turns plugin management on (ENABLE_PLUGIN_INSTALLATION). Off by default.
	Enabled bool
	// AllowInsecureEndpoints permits http, loopback and private endpoint URLs
	// (ALLOW_INSECURE_PLUGIN_ENDPOINTS). For local development only.
	AllowInsecureEndpoints bool

	ControllerURL   string // PLUGIN_CONTROLLER_URL; empty disables image and repository installs
	ControllerToken string // PLUGIN_CONTROLLER_TOKEN

	BuildTimeout  time.Duration // PLUGIN_BUILD_TIMEOUT
	HealthTimeout time.Duration // PLUGIN_HEALTH_TIMEOUT
	CPULimit      float64       // PLUGIN_CPU_LIMIT (CPUs)
	MemoryLimit   int64         // PLUGIN_MEMORY_LIMIT (bytes)

	// RollbackWindow is how long the previous plugin's instance is kept after a switch.
	RollbackWindow time.Duration
	// ActivationTimeout bounds one activation including migration.
	ActivationTimeout time.Duration
	// QueueDrainWindow bounds how long an old queue keeps being consumed after a switch.
	QueueDrainWindow time.Duration
	// MaxReembedEntries bounds the re-embedding migration.
	MaxReembedEntries int
	// RestoreWait is how long startup keeps retrying an unreachable plugin or
	// controller before giving up (PLUGIN_RESTORE_WAIT).
	RestoreWait time.Duration
}

// Defaults fills unset fields.
func (c *Config) Defaults() {
	if c.BuildTimeout <= 0 {
		c.BuildTimeout = 10 * time.Minute
	}
	if c.HealthTimeout <= 0 {
		c.HealthTimeout = 60 * time.Second
	}
	if c.CPULimit <= 0 {
		c.CPULimit = 1
	}
	if c.MemoryLimit <= 0 {
		c.MemoryLimit = 512 << 20
	}
	if c.RollbackWindow <= 0 {
		c.RollbackWindow = 10 * time.Minute
	}
	if c.ActivationTimeout <= 0 {
		c.ActivationTimeout = 10 * time.Minute
	}
	if c.QueueDrainWindow <= 0 {
		c.QueueDrainWindow = 30 * time.Second
	}
	if c.MaxReembedEntries <= 0 {
		c.MaxReembedEntries = 50_000
	}
	if c.RestoreWait <= 0 {
		c.RestoreWait = 2 * time.Minute
	}
}

// Host is what the manager needs from the running orchestrator.
type Host interface {
	// PauseWrites blocks cache writes until the returned function is called.
	PauseWrites(ctx context.Context) (resume func(), err error)
	// FlushCache empties vector store, persisted entries and policy state.
	FlushCache(ctx context.Context) error
	Threshold() float64
	SetThreshold(float64)
	CacheSize() (int, error)
}

// ---- controller contract -----------------------------------------------------

// Controller is the manager's view of the plugin controller. The local
// implementation drives Docker; a Kubernetes implementation can replace it.
type Controller interface {
	// Prepare resolves an image to an immutable digest or builds a repository at
	// an exact revision, scans the result, and returns the parsed manifest.
	Prepare(ctx context.Context, req PrepareRequest) (*PrepareResult, error)
	// Start runs (or re-uses) an isolated instance of a prepared image.
	Start(ctx context.Context, req StartRequest) (*Instance, error)
	Stop(ctx context.Context, instanceID string) error
	Logs(ctx context.Context, instanceID string, tail int) ([]string, error)
	Health(ctx context.Context) error
}

// The wire types are shared with the controller service.
type (
	PrepareRequest = ctlapi.PrepareRequest
	PrepareResult  = ctlapi.PrepareResult
	StartRequest   = ctlapi.StartRequest
	Instance       = ctlapi.Instance
)

// ---- requests ----------------------------------------------------------------

// InstallRequest is the body of verify and install.
type InstallRequest struct {
	Type     string `json:"type"`
	Mode     string `json:"mode"` // endpoint | image | repository
	Name     string `json:"name,omitempty"`
	Version  string `json:"version,omitempty"`
	Manifest string `json:"manifest,omitempty"` // plugin.yaml text

	Endpoint string `json:"endpoint,omitempty"`
	Image    string `json:"image,omitempty"`
	Repo     string `json:"repo,omitempty"`
	Revision string `json:"revision,omitempty"`

	Config  map[string]any    `json:"config,omitempty"`
	Secrets map[string]string `json:"secrets,omitempty"` // write-only

	// Activate makes install continue into activation once verified.
	Activate      bool                    `json:"activate,omitempty"`
	Confirmations *registry.Confirmations `json:"confirmations,omitempty"`
}

// EndpointTokenSecret is the optional credential a hosted endpoint is called with.
const EndpointTokenSecret = "ENDPOINT_TOKEN"

// ---- errors ------------------------------------------------------------------

var (
	ErrDisabled = errors.New("plugin management is disabled")
	ErrBusy     = errors.New("another operation is in progress for this plugin or component")
	ErrNoHost   = errors.New("orchestrator host is not attached")
)

// RequirementError means activation needs an explicit administrator decision.
type RequirementError struct {
	Field   string   `json:"field"`
	Options []string `json:"options,omitempty"`
	Message string   `json:"message"`
	Warning string   `json:"warning,omitempty"`
}

func (e *RequirementError) Error() string { return e.Message }

// IncompatibleError means the candidate cannot be activated in this state.
type IncompatibleError struct{ Reason string }

func (e *IncompatibleError) Error() string { return e.Reason }

// InvalidError is a problem with the request itself.
type InvalidError struct{ Msg string }

func (e *InvalidError) Error() string { return e.Msg }

func invalidf(format string, a ...any) error { return &InvalidError{fmt.Sprintf(format, a...)} }

// ---- views -------------------------------------------------------------------

// SecretStatus reports that a secret exists, never its value.
type SecretStatus struct {
	Name       string `json:"name"`
	Configured bool   `json:"configured"`
	Required   bool   `json:"required,omitempty"`
}

// View is the API representation of a record. It contains no secret material:
// not plaintext, not ciphertext, not nonces.
type View struct {
	ID               string                       `json:"id"`
	Name             string                       `json:"name"`
	Version          string                       `json:"version"`
	Type             plugins.Type                 `json:"type"`
	Slot             plugins.Type                 `json:"slot"`
	ContractVersion  string                       `json:"contract_version"`
	Mode             plugins.Mode                 `json:"mode"`
	Source           registry.Source              `json:"source"`
	Commit           string                       `json:"commit,omitempty"`
	ImageDigest      string                       `json:"image_digest,omitempty"`
	Description      string                       `json:"description,omitempty"`
	ConfigSchema     map[string]manifest.Property `json:"config_schema,omitempty"`
	Config           map[string]any               `json:"config,omitempty"`
	Secrets          []SecretStatus               `json:"secrets"`
	State            plugins.State                `json:"state"`
	StateDetail      string                       `json:"state_detail,omitempty"`
	Health           registry.Health              `json:"health"`
	Active           bool                         `json:"active"`
	PreviousActiveID string                       `json:"previous_active_id,omitempty"`
	UpgradedFromID   string                       `json:"upgraded_from_id,omitempty"`
	Confirmations    registry.Confirmations       `json:"confirmations"`
	Verification     *registry.Verification       `json:"verification,omitempty"`
	Scan             *registry.ScanReport         `json:"scan,omitempty"`
	Provenance       *registry.Provenance         `json:"provenance,omitempty"`
	CreatedAt        time.Time                    `json:"created_at"`
	UpdatedAt        time.Time                    `json:"updated_at"`
}

// ViewOf builds the API view of a record.
func ViewOf(r *registry.Record) View {
	v := View{
		ID: r.ID, Name: r.Name, Version: r.Version, Type: r.Type, Slot: r.Type.Slot(),
		ContractVersion: r.ContractVersion, Mode: r.Mode, Source: r.Source, Commit: r.Commit,
		ImageDigest: r.ImageDigest, Config: r.Config, State: r.State, StateDetail: r.StateDetail,
		Health: r.Health, Active: r.Active, PreviousActiveID: r.PreviousActiveID,
		UpgradedFromID: r.UpgradedFromID, Confirmations: r.Confirmations,
		Verification: r.Verification, Scan: r.Scan, Provenance: r.Provenance,
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt, Secrets: []SecretStatus{},
	}
	declared := map[string]manifest.SecretRef{}
	if r.Manifest != nil {
		v.Description = r.Manifest.Metadata.Description
		v.ConfigSchema = r.Manifest.Spec.Config.Properties
		for _, s := range r.Manifest.Spec.Secrets {
			declared[s.Name] = s
		}
	}
	seen := map[string]bool{}
	for _, s := range declared {
		_, has := r.Secrets[s.Name]
		v.Secrets = append(v.Secrets, SecretStatus{Name: s.Name, Configured: has, Required: s.Required})
		seen[s.Name] = true
	}
	for n := range r.Secrets {
		if !seen[n] {
			v.Secrets = append(v.Secrets, SecretStatus{Name: n, Configured: true})
		}
	}
	sortSecretStatus(v.Secrets)
	return v
}

func sortSecretStatus(s []SecretStatus) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && strings.Compare(s[j-1].Name, s[j].Name) > 0; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}
