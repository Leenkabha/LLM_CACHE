// Package manifest parses and validates plugin.yaml, the strict, versioned
// description of an installable plugin.
//
// Validation is deliberately closed-world: unknown fields are errors, fields
// that would widen the container's privileges get a specific rejection message,
// and nothing that looks like a secret may be embedded in the file. The manifest
// is data only -- it can never carry a command to run.
package manifest

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/leenkabha/llm_cache/internal/plugins"
)

const (
	// APIVersion is the only manifest schema version this release accepts.
	APIVersion = "llmcache.dev/v1alpha1"
	// Kind is the only object kind.
	Kind = "Plugin"
	// MaxBytes bounds a manifest file.
	MaxBytes = 64 << 10

	RuntimeContainer = "container"
	RuntimePython    = "python"
	RuntimeEndpoint  = "endpoint"

	EgressNone     = "none"
	EgressInternet = "internet"

	// Absolute ceilings; the operator's PLUGIN_CPU_LIMIT/PLUGIN_MEMORY_LIMIT are
	// enforced on top of these by the controller.
	MaxCPU        = 4.0
	MaxMemory     = int64(4) << 30
	MaxPIDs       = 1024
	MaxTmpfs      = int64(1) << 30
	DefaultPort   = 8080
	DefaultHealth = "/health"
)

// Manifest is the parsed plugin.yaml.
type Manifest struct {
	APIVersion string   `yaml:"apiVersion" json:"apiVersion"`
	Kind       string   `yaml:"kind" json:"kind"`
	Metadata   Metadata `yaml:"metadata" json:"metadata"`
	Spec       Spec     `yaml:"spec" json:"spec"`
}

type Metadata struct {
	Name        string `yaml:"name" json:"name"`
	Version     string `yaml:"version" json:"version"`
	Description string `yaml:"description,omitempty" json:"description,omitempty"`
}

type Spec struct {
	Type      string      `yaml:"type" json:"type"`
	Contract  Contract    `yaml:"contract,omitempty" json:"contract"`
	Runtime   Runtime     `yaml:"runtime,omitempty" json:"runtime"`
	Python    *Python     `yaml:"python,omitempty" json:"python,omitempty"`
	Health    Health      `yaml:"health,omitempty" json:"health"`
	Config    Config      `yaml:"config,omitempty" json:"config"`
	Secrets   []SecretRef `yaml:"secrets,omitempty" json:"secrets,omitempty"`
	Resources Resources   `yaml:"resources,omitempty" json:"resources"`
	Network   Network     `yaml:"network,omitempty" json:"network"`
	Verify    Verify      `yaml:"verify,omitempty" json:"verify"`
}

// Contract pins the remote-protocol version the plugin implements.
type Contract struct {
	Version string `yaml:"version,omitempty" json:"version"`
}

// Runtime describes how a container plugin is built and started. There is no
// command or entrypoint field on purpose: the image's own entrypoint is used.
type Runtime struct {
	Mode       string `yaml:"mode,omitempty" json:"mode"`
	Dockerfile string `yaml:"dockerfile,omitempty" json:"dockerfile,omitempty"`
	Context    string `yaml:"context,omitempty" json:"context,omitempty"`
	Port       int    `yaml:"port,omitempty" json:"port,omitempty"`
}

// Python configures the Python registry plugin types.
type Python struct {
	Module       string `yaml:"module" json:"module"`                 // package or module name inside Path
	Path         string `yaml:"path,omitempty" json:"path,omitempty"` // directory holding Module, default "."
	Backend      string `yaml:"backend" json:"backend"`               // name the plugin passes to @register
	Requirements string `yaml:"requirements,omitempty" json:"requirements,omitempty"`
}

type Health struct {
	Path    string `yaml:"path,omitempty" json:"path"`
	Timeout string `yaml:"timeout,omitempty" json:"timeout"`
}

type Config struct {
	Properties map[string]Property `yaml:"properties,omitempty" json:"properties,omitempty"`
}

// Property describes one non-secret configuration value.
type Property struct {
	Type        string   `yaml:"type" json:"type"` // string integer number boolean duration enum
	Required    bool     `yaml:"required,omitempty" json:"required,omitempty"`
	Default     any      `yaml:"default,omitempty" json:"default,omitempty"`
	Description string   `yaml:"description,omitempty" json:"description,omitempty"`
	Enum        []string `yaml:"enum,omitempty" json:"enum,omitempty"`
	Min         *float64 `yaml:"min,omitempty" json:"min,omitempty"`
	Max         *float64 `yaml:"max,omitempty" json:"max,omitempty"`
}

// SecretRef declares a secret the plugin needs. Only the name is ever stored in
// the manifest; the value is entered separately and encrypted at rest.
type SecretRef struct {
	Name        string `yaml:"name" json:"name"`
	Required    bool   `yaml:"required,omitempty" json:"required,omitempty"`
	Description string `yaml:"description,omitempty" json:"description,omitempty"`
}

type Resources struct {
	CPU    string `yaml:"cpu,omitempty" json:"cpu,omitempty"`       // "500m" or "0.5"
	Memory string `yaml:"memory,omitempty" json:"memory,omitempty"` // "256Mi"
	PIDs   int    `yaml:"pids,omitempty" json:"pids,omitempty"`
	Tmpfs  string `yaml:"tmpfs,omitempty" json:"tmpfs,omitempty"`
}

type Network struct {
	Egress string `yaml:"egress,omitempty" json:"egress,omitempty"` // none (default) | internet
}

// Verify carries hints for the contract tests.
type Verify struct {
	VectorDim int `yaml:"vectorDim,omitempty" json:"vectorDim,omitempty"`
}

// Issue is one validation problem.
type Issue struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

// ValidationError lists every problem found, so a developer can fix them in one
// pass.
type ValidationError struct{ Issues []Issue }

func (e *ValidationError) Error() string {
	parts := make([]string, len(e.Issues))
	for i, is := range e.Issues {
		if is.Path == "" {
			parts[i] = is.Message
		} else {
			parts[i] = is.Path + ": " + is.Message
		}
	}
	return "invalid plugin manifest: " + strings.Join(parts, "; ")
}

var (
	nameRE    = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	semverRE  = regexp.MustCompile(`^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?(\+[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$`)
	envRE     = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)
	propRE    = regexp.MustCompile(`^[a-z][a-zA-Z0-9_]{0,63}$`)
	pyModRE   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)
	backendRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
)

// reservedEnv are names the platform owns; a secret may not shadow them.
var reservedEnv = map[string]bool{
	"PATH": true, "HOME": true, "USER": true, "PORT": true, "HOSTNAME": true,
	"LD_PRELOAD": true, "LD_LIBRARY_PATH": true, "PYTHONPATH": true, "PYTHONHOME": true,
	"EMBEDDING_MODEL_BACKEND": true, "VECTOR_INDEX_BACKEND": true, "SIMILARITY_METRIC": true,
	"VECTOR_DIM": true,
}

// dangerousKeys are field names that would widen a container's privileges (or
// run arbitrary commands). They are matched case-insensitively with '_' and '-'
// removed, at any depth, so a rename such as host_network still trips.
var dangerousKeys = map[string]string{
	"privileged":               "privileged containers are not allowed",
	"hostnetwork":              "host networking is not allowed",
	"networkmode":              "host networking is not allowed",
	"hostpid":                  "the host PID namespace is not allowed",
	"pid":                      "the host PID namespace is not allowed",
	"hostipc":                  "the host IPC namespace is not allowed",
	"ipc":                      "the host IPC namespace is not allowed",
	"volumes":                  "host filesystem mounts are not allowed",
	"volume":                   "host filesystem mounts are not allowed",
	"mounts":                   "host filesystem mounts are not allowed",
	"hostpath":                 "host filesystem mounts are not allowed",
	"binds":                    "host filesystem mounts are not allowed",
	"devices":                  "host devices are not allowed",
	"dockersocket":             "mounting the Docker socket is not allowed",
	"capabilities":             "adding capabilities is not allowed (all are dropped)",
	"capadd":                   "adding capabilities is not allowed (all are dropped)",
	"capdrop":                  "capabilities are managed by the platform (all are dropped)",
	"securitycontext":          "security settings are managed by the platform",
	"securityopt":              "security settings are managed by the platform",
	"sysctls":                  "sysctls are not allowed",
	"user":                     "the platform always runs plugins as a non-root user",
	"runasuser":                "the platform always runs plugins as a non-root user",
	"command":                  "custom commands are not allowed; the image entrypoint is used",
	"entrypoint":               "custom entrypoints are not allowed; the image entrypoint is used",
	"args":                     "custom arguments are not allowed; the image entrypoint is used",
	"env":                      "declare configuration under spec.config and secrets under spec.secrets",
	"environment":              "declare configuration under spec.config and secrets under spec.secrets",
	"buildargs":                "build arguments are not allowed (secrets must never reach the build)",
	"allowprivilegeescalation": "privilege escalation is not allowed",
}

// secretPatterns catch credentials pasted into a manifest.
var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`sk-[A-Za-z0-9_-]{16,}`),
	regexp.MustCompile(`AIza[0-9A-Za-z_-]{20,}`),
	regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{20,}`),
	regexp.MustCompile(`github_pat_[A-Za-z0-9_]{20,}`),
	regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
	regexp.MustCompile(`xox[baprs]-[A-Za-z0-9-]{10,}`),
	regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`),
	regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._~+/=-]{16,}`),
	regexp.MustCompile(`(?i)://[^/\s:@]+:[^/\s@]+@`), // credentials in a URL
}

var secretNameHint = regexp.MustCompile(`(?i)(token|secret|password|passwd|api_?key|apikey|credential|private_?key)`)

// Parse decodes and validates a manifest. The returned manifest has defaults
// applied.
func Parse(data []byte) (*Manifest, error) {
	if len(data) == 0 {
		return nil, &ValidationError{[]Issue{{"", "manifest is empty"}}}
	}
	if len(data) > MaxBytes {
		return nil, &ValidationError{[]Issue{{"", fmt.Sprintf("manifest exceeds %d bytes", MaxBytes)}}}
	}

	var generic any
	if err := yaml.Unmarshal(data, &generic); err != nil {
		return nil, &ValidationError{[]Issue{{"", "not valid YAML: " + firstLine(err.Error())}}}
	}
	var issues []Issue
	scanDangerous(generic, "", &issues)
	if len(issues) > 0 {
		return nil, &ValidationError{issues}
	}

	var m Manifest
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&m); err != nil && !errors.Is(err, io.EOF) {
		var te *yaml.TypeError
		if errors.As(err, &te) {
			for _, msg := range te.Errors {
				issues = append(issues, Issue{"", "invalid manifest structure: " + msg})
			}
			return nil, &ValidationError{issues}
		}
		return nil, &ValidationError{[]Issue{{"", "invalid manifest structure: " + firstLine(err.Error())}}}
	}
	// A manifest must be a single document.
	var extra any
	if err := dec.Decode(&extra); err == nil {
		return nil, &ValidationError{[]Issue{{"", "manifest must contain exactly one YAML document"}}}
	}

	m.applyDefaults()
	if issues := m.validate(); len(issues) > 0 {
		return nil, &ValidationError{issues}
	}
	return &m, nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}

func normKey(k string) string {
	k = strings.ToLower(k)
	k = strings.ReplaceAll(k, "_", "")
	return strings.ReplaceAll(k, "-", "")
}

func scanDangerous(node any, at string, issues *[]Issue) {
	switch v := node.(type) {
	case map[string]any:
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			p := k
			if at != "" {
				p = at + "." + k
			}
			// Keys directly under spec.config.properties are developer-chosen
			// property names, not platform fields, so "user" or "env" are fine there.
			if at != "spec.config.properties" {
				if msg, bad := dangerousKeys[normKey(k)]; bad {
					*issues = append(*issues, Issue{p, msg})
				}
			}
			scanDangerous(v[k], p, issues)
		}
	case []any:
		for i, e := range v {
			scanDangerous(e, fmt.Sprintf("%s[%d]", at, i), issues)
		}
	}
}

func (m *Manifest) applyDefaults() {
	if m.Spec.Contract.Version == "" {
		m.Spec.Contract.Version = plugins.ContractVersion
	}
	rt := &m.Spec.Runtime
	if rt.Mode == "" {
		if t, err := plugins.ParseType(m.Spec.Type); err == nil && t.IsPython() {
			rt.Mode = RuntimePython
		} else {
			rt.Mode = RuntimeContainer
		}
	}
	if rt.Mode == RuntimeContainer {
		if rt.Dockerfile == "" {
			rt.Dockerfile = "Dockerfile"
		}
		if rt.Context == "" {
			rt.Context = "."
		}
	}
	if rt.Port == 0 && rt.Mode != RuntimeEndpoint {
		rt.Port = DefaultPort
	}
	if m.Spec.Python != nil && m.Spec.Python.Path == "" {
		m.Spec.Python.Path = "."
	}
	if m.Spec.Health.Path == "" {
		m.Spec.Health.Path = DefaultHealth
	}
	if m.Spec.Health.Timeout == "" {
		m.Spec.Health.Timeout = "5s"
	}
	if m.Spec.Network.Egress == "" {
		m.Spec.Network.Egress = EgressNone
	}
}

func (m *Manifest) validate() []Issue {
	var issues []Issue
	add := func(p, format string, a ...any) { issues = append(issues, Issue{p, fmt.Sprintf(format, a...)}) }

	if m.APIVersion != APIVersion {
		add("apiVersion", "unsupported apiVersion %q (supported: %s)", m.APIVersion, APIVersion)
	}
	if m.Kind != Kind {
		add("kind", "kind must be %q, got %q", Kind, m.Kind)
	}
	if !nameRE.MatchString(m.Metadata.Name) {
		add("metadata.name", "must be 1-63 lowercase letters, digits or '-', starting and ending with an alphanumeric")
	}
	if !semverRE.MatchString(m.Metadata.Version) {
		add("metadata.version", "must be a semantic version such as 1.2.3")
	}
	if len(m.Metadata.Description) > 500 {
		add("metadata.description", "must be at most 500 characters")
	}

	typ, err := plugins.ParseType(m.Spec.Type)
	if err != nil {
		add("spec.type", "%v", err)
	}
	if m.Spec.Contract.Version != plugins.ContractVersion {
		add("spec.contract.version", "unsupported contract version %q (supported: %s)", m.Spec.Contract.Version, plugins.ContractVersion)
	}

	// Runtime.
	rt := m.Spec.Runtime
	switch rt.Mode {
	case RuntimeContainer:
		if err == nil && typ.IsPython() {
			add("spec.runtime.mode", "type %s is a Python registry plugin; use mode %q", typ, RuntimePython)
		}
		checkRelPath(&issues, "spec.runtime.dockerfile", rt.Dockerfile)
		checkRelPath(&issues, "spec.runtime.context", rt.Context)
	case RuntimePython:
		if err == nil && !typ.IsPython() {
			add("spec.runtime.mode", "mode %q is only valid for embedding-model, vector-index and similarity-metric", RuntimePython)
		}
		if rt.Dockerfile != "" || rt.Context != "" {
			add("spec.runtime", "python plugins are built from a platform template; dockerfile/context are not allowed")
		}
	case RuntimeEndpoint:
		// no build inputs at all
		if rt.Dockerfile != "" || rt.Context != "" {
			add("spec.runtime", "endpoint plugins have nothing to build")
		}
	default:
		add("spec.runtime.mode", "unsupported mode %q (supported: container, python, endpoint)", rt.Mode)
	}
	if rt.Mode != RuntimeEndpoint && (rt.Port < 1024 || rt.Port > 65535) {
		add("spec.runtime.port", "must be between 1024 and 65535 (plugins run unprivileged)")
	}

	// Python.
	if rt.Mode == RuntimePython {
		if m.Spec.Python == nil {
			add("spec.python", "required for python plugins")
		} else {
			p := m.Spec.Python
			if !pyModRE.MatchString(p.Module) {
				add("spec.python.module", "must be a valid Python identifier")
			}
			if !backendRE.MatchString(p.Backend) {
				add("spec.python.backend", "must be the lowercase name passed to @register (letters, digits, '.', '_', '-')")
			}
			checkRelPath(&issues, "spec.python.path", p.Path)
			if p.Requirements != "" {
				checkRelPath(&issues, "spec.python.requirements", p.Requirements)
			}
		}
	} else if m.Spec.Python != nil {
		add("spec.python", "only valid when runtime.mode is python")
	}

	// Health.
	hp := m.Spec.Health.Path
	if !strings.HasPrefix(hp, "/") || strings.Contains(hp, "..") || strings.ContainsAny(hp, "?#\\ ") || strings.Contains(hp, "//") || len(hp) > 200 {
		add("spec.health.path", "must be an absolute path such as /health without query, fragment or '..'")
	}
	if d, e := time.ParseDuration(m.Spec.Health.Timeout); e != nil || d < 100*time.Millisecond || d > time.Minute {
		add("spec.health.timeout", "must be a duration between 100ms and 60s")
	}

	// Config.
	if len(m.Spec.Config.Properties) > 64 {
		add("spec.config.properties", "at most 64 properties")
	}
	for name, prop := range m.Spec.Config.Properties {
		p := "spec.config.properties." + name
		if !propRE.MatchString(name) {
			add(p, "property names must match %s", propRE)
		}
		if secretNameHint.MatchString(name) {
			add(p, "looks like a secret; declare it under spec.secrets so it is stored encrypted")
		}
		checkProperty(&issues, p, prop)
	}

	// Secrets.
	seen := map[string]bool{}
	if len(m.Spec.Secrets) > 32 {
		add("spec.secrets", "at most 32 secrets")
	}
	for i, s := range m.Spec.Secrets {
		p := fmt.Sprintf("spec.secrets[%d]", i)
		switch {
		case !envRE.MatchString(s.Name):
			add(p+".name", "must be an environment-variable style name such as API_TOKEN")
		case reservedEnv[s.Name]:
			add(p+".name", "%s is reserved by the platform", s.Name)
		case seen[s.Name]:
			add(p+".name", "duplicate secret %s", s.Name)
		}
		seen[s.Name] = true
	}

	// Resources.
	res := m.Spec.Resources
	if res.CPU != "" {
		if c, e := ParseCPU(res.CPU); e != nil {
			add("spec.resources.cpu", "%v", e)
		} else if c > MaxCPU {
			add("spec.resources.cpu", "requests %.2f CPUs; the maximum is %.0f", c, MaxCPU)
		}
	}
	if res.Memory != "" {
		if b, e := ParseBytes(res.Memory); e != nil {
			add("spec.resources.memory", "%v", e)
		} else if b > MaxMemory || b < 16<<20 {
			add("spec.resources.memory", "must be between 16Mi and 4Gi")
		}
	}
	if res.Tmpfs != "" {
		if b, e := ParseBytes(res.Tmpfs); e != nil {
			add("spec.resources.tmpfs", "%v", e)
		} else if b > MaxTmpfs || b < 1<<20 {
			add("spec.resources.tmpfs", "must be between 1Mi and 1Gi")
		}
	}
	if res.PIDs < 0 || res.PIDs > MaxPIDs {
		add("spec.resources.pids", "must be between 0 and %d", MaxPIDs)
	}

	// Network.
	if m.Spec.Network.Egress != EgressNone && m.Spec.Network.Egress != EgressInternet {
		add("spec.network.egress", "must be %q (default) or %q", EgressNone, EgressInternet)
	}
	if m.Spec.Verify.VectorDim < 0 || m.Spec.Verify.VectorDim > 8192 {
		add("spec.verify.vectorDim", "must be between 0 and 8192")
	}

	// No credential-looking strings anywhere in the file.
	for _, s := range m.strings() {
		for _, re := range secretPatterns {
			if re.MatchString(s.value) {
				add(s.path, "contains what looks like a credential; never put secret values in plugin.yaml (declare a secret name instead)")
				break
			}
		}
	}
	return issues
}

type located struct{ path, value string }

func (m *Manifest) strings() []located {
	out := []located{{"metadata.description", m.Metadata.Description}}
	for name, p := range m.Spec.Config.Properties {
		base := "spec.config.properties." + name
		out = append(out, located{base + ".description", p.Description})
		if s, ok := p.Default.(string); ok {
			out = append(out, located{base + ".default", s})
		}
		for i, e := range p.Enum {
			out = append(out, located{fmt.Sprintf("%s.enum[%d]", base, i), e})
		}
	}
	for i, s := range m.Spec.Secrets {
		out = append(out, located{fmt.Sprintf("spec.secrets[%d].description", i), s.Description})
	}
	return out
}

func checkRelPath(issues *[]Issue, at, p string) {
	bad := func(why string) { *issues = append(*issues, Issue{at, why}) }
	switch {
	case p == "":
		bad("must not be empty")
	case len(p) > 200:
		bad("too long")
	case strings.ContainsAny(p, "\\\x00") || strings.ContainsFunc(p, func(r rune) bool { return r < 0x20 }):
		bad("contains invalid characters")
	case strings.HasPrefix(p, "/") || regexp.MustCompile(`^[A-Za-z]:`).MatchString(p):
		bad("must be relative to the repository root")
	case strings.HasPrefix(p, "~"):
		bad("must not start with '~'")
	default:
		clean := path.Clean(p)
		if clean == ".." || strings.HasPrefix(clean, "../") || strings.Contains(p, "..") {
			bad("must stay inside the repository (no '..')")
		}
	}
}

func checkProperty(issues *[]Issue, p string, prop Property) {
	add := func(format string, a ...any) { *issues = append(*issues, Issue{p, fmt.Sprintf(format, a...)}) }
	switch prop.Type {
	case "string", "integer", "number", "boolean", "duration":
	case "enum":
		if len(prop.Enum) == 0 {
			add("enum properties need a non-empty enum list")
		}
	default:
		add("unsupported type %q (supported: string, integer, number, boolean, duration, enum)", prop.Type)
		return
	}
	if prop.Type != "enum" && len(prop.Enum) > 0 {
		add("enum values are only valid for type enum")
	}
	if prop.Default != nil {
		if prop.Required {
			add("a required property cannot have a default")
		}
		if _, err := CoerceValue(prop, prop.Default); err != nil {
			add("default: %v", err)
		}
	}
}

// ---- value handling ----------------------------------------------------------

// CoerceValue checks v against a property and returns its canonical Go value
// (string, int64, float64, bool). Durations are returned as their string form.
func CoerceValue(prop Property, v any) (any, error) {
	switch prop.Type {
	case "string":
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("must be a string")
		}
		if len(s) > 4096 {
			return nil, fmt.Errorf("must be at most 4096 characters")
		}
		return s, nil
	case "enum":
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("must be a string")
		}
		for _, e := range prop.Enum {
			if e == s {
				return s, nil
			}
		}
		return nil, fmt.Errorf("must be one of %v", prop.Enum)
	case "boolean":
		b, ok := v.(bool)
		if !ok {
			return nil, fmt.Errorf("must be true or false")
		}
		return b, nil
	case "integer":
		f, ok := toFloat(v)
		if !ok || f != float64(int64(f)) {
			return nil, fmt.Errorf("must be an integer")
		}
		if err := inRange(prop, f); err != nil {
			return nil, err
		}
		return int64(f), nil
	case "number":
		f, ok := toFloat(v)
		if !ok {
			return nil, fmt.Errorf("must be a number")
		}
		if err := inRange(prop, f); err != nil {
			return nil, err
		}
		return f, nil
	case "duration":
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("must be a duration string such as 30s")
		}
		d, err := time.ParseDuration(s)
		if err != nil || d <= 0 {
			return nil, fmt.Errorf("must be a positive duration such as 30s")
		}
		return s, nil
	}
	return nil, fmt.Errorf("unsupported type %q", prop.Type)
}

func inRange(prop Property, f float64) error {
	if prop.Min != nil && f < *prop.Min {
		return fmt.Errorf("must be at least %v", *prop.Min)
	}
	if prop.Max != nil && f > *prop.Max {
		return fmt.Errorf("must be at most %v", *prop.Max)
	}
	return nil
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case float64:
		return n, true
	case float32:
		return float64(n), true
	}
	return 0, false
}

// ResolveConfig validates administrator-supplied values against the manifest,
// rejects unknown keys, fills defaults and enforces required properties.
func (m *Manifest) ResolveConfig(values map[string]any) (map[string]any, error) {
	out := map[string]any{}
	var problems []string
	for k := range values {
		if _, ok := m.Spec.Config.Properties[k]; !ok {
			problems = append(problems, fmt.Sprintf("unknown configuration key %q", k))
		}
	}
	names := make([]string, 0, len(m.Spec.Config.Properties))
	for n := range m.Spec.Config.Properties {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		prop := m.Spec.Config.Properties[n]
		v, given := values[n]
		if !given || v == nil || v == "" {
			switch {
			case prop.Default != nil:
				cv, _ := CoerceValue(prop, prop.Default)
				out[n] = cv
			case prop.Required:
				problems = append(problems, fmt.Sprintf("configuration %q is required", n))
			}
			continue
		}
		cv, err := CoerceValue(prop, v)
		if err != nil {
			problems = append(problems, fmt.Sprintf("configuration %q %v", n, err))
			continue
		}
		out[n] = cv
	}
	if len(problems) > 0 {
		return nil, errors.New(strings.Join(problems, "; "))
	}
	return out, nil
}

// CheckSecrets verifies that the supplied secret names are declared and that
// every required secret is present. Values are not inspected here.
func (m *Manifest) CheckSecrets(names []string) error {
	declared := map[string]SecretRef{}
	for _, s := range m.Spec.Secrets {
		declared[s.Name] = s
	}
	have := map[string]bool{}
	var problems []string
	for _, n := range names {
		if _, ok := declared[n]; !ok {
			problems = append(problems, fmt.Sprintf("secret %q is not declared in the manifest", n))
		}
		have[n] = true
	}
	for _, s := range m.Spec.Secrets {
		if s.Required && !have[s.Name] {
			problems = append(problems, fmt.Sprintf("secret %q is required", s.Name))
		}
	}
	sort.Strings(problems)
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

// HealthTimeout returns the parsed health timeout (validated by Parse).
func (m *Manifest) HealthTimeout() time.Duration {
	d, err := time.ParseDuration(m.Spec.Health.Timeout)
	if err != nil {
		return 5 * time.Second
	}
	return d
}

// ---- resource quantities -----------------------------------------------------

// ParseCPU accepts "0.5", "2" or "500m" and returns CPUs.
func ParseCPU(s string) (float64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("cpu must not be empty")
	}
	milli := strings.HasSuffix(s, "m")
	num := strings.TrimSuffix(s, "m")
	f, err := strconv.ParseFloat(num, 64)
	if err != nil || f <= 0 || f != f {
		return 0, fmt.Errorf("invalid cpu %q (use e.g. 500m or 0.5)", s)
	}
	if milli {
		f /= 1000
	}
	if f < 0.01 {
		return 0, fmt.Errorf("cpu %q is below 10m", s)
	}
	return f, nil
}

// ParseBytes accepts a positive integer with an optional Ki/Mi/Gi (binary) or
// K/M/G (decimal) suffix, e.g. "256Mi".
func ParseBytes(s string) (int64, error) {
	s = strings.TrimSpace(s)
	units := []struct {
		suffix string
		mult   int64
	}{{"Ki", 1 << 10}, {"Mi", 1 << 20}, {"Gi", 1 << 30}, {"K", 1000}, {"M", 1000 * 1000}, {"G", 1000 * 1000 * 1000}}
	mult := int64(1)
	num := s
	for _, u := range units {
		if strings.HasSuffix(s, u.suffix) {
			mult, num = u.mult, strings.TrimSuffix(s, u.suffix)
			break
		}
	}
	n, err := strconv.ParseInt(num, 10, 64)
	if err != nil || n <= 0 || n > (1<<40) {
		return 0, fmt.Errorf("invalid size %q (use e.g. 256Mi)", s)
	}
	return n * mult, nil
}

// Effective returns the resource limits after applying operator defaults and
// ceilings. It fails when the manifest asks for more than the operator allows.
func (m *Manifest) Effective(defCPU float64, defMem int64, maxCPU float64, maxMem int64) (cpu float64, mem int64, pids int, tmpfs int64, err error) {
	cpu, mem, pids, tmpfs = defCPU, defMem, 128, 64<<20
	if m.Spec.Resources.CPU != "" {
		cpu, _ = ParseCPU(m.Spec.Resources.CPU)
	}
	if m.Spec.Resources.Memory != "" {
		mem, _ = ParseBytes(m.Spec.Resources.Memory)
	}
	if m.Spec.Resources.PIDs > 0 {
		pids = m.Spec.Resources.PIDs
	}
	if m.Spec.Resources.Tmpfs != "" {
		tmpfs, _ = ParseBytes(m.Spec.Resources.Tmpfs)
	}
	if maxCPU > 0 && cpu > maxCPU {
		return 0, 0, 0, 0, fmt.Errorf("plugin requests %.2f CPUs but this installation allows at most %.2f (PLUGIN_CPU_LIMIT)", cpu, maxCPU)
	}
	if maxMem > 0 && mem > maxMem {
		return 0, 0, 0, 0, fmt.Errorf("plugin requests %d bytes of memory but this installation allows at most %d (PLUGIN_MEMORY_LIMIT)", mem, maxMem)
	}
	return cpu, mem, pids, tmpfs, nil
}

// Minimal builds the manifest used for hosted endpoints that arrive without a
// plugin.yaml.
func Minimal(name, version string, t plugins.Type) *Manifest {
	m := &Manifest{
		APIVersion: APIVersion, Kind: Kind,
		Metadata: Metadata{Name: name, Version: version},
		Spec:     Spec{Type: string(t), Runtime: Runtime{Mode: RuntimeEndpoint}},
	}
	if t == plugins.TypeLLM {
		// The two settings the LLM adapter understands, so the UI can offer them
		// for a hosted endpoint that arrives without a plugin.yaml.
		m.Spec.Config.Properties = map[string]Property{
			"model":           {Type: "string", Default: "default", Description: "model name sent in every request"},
			"request_timeout": {Type: "duration", Default: "60s", Description: "per-request timeout"},
		}
	}
	m.applyDefaults()
	return m
}

// Validate re-validates a manifest constructed in code (used by Minimal callers).
func (m *Manifest) Validate() error {
	if issues := m.validate(); len(issues) > 0 {
		return &ValidationError{issues}
	}
	return nil
}
