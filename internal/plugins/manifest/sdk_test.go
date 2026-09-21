package manifest_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/leenkabha/llm_cache/internal/plugins"
	. "github.com/leenkabha/llm_cache/internal/plugins/manifest"
	"github.com/leenkabha/llm_cache/internal/plugins/testutil"
)

// Every manifest shipped in the SDK must be valid, and there must be one example
// per plugin type.
func TestSDKManifestsAreValid(t *testing.T) {
	root := testutil.RepoRoot()
	for _, typ := range plugins.AllTypes {
		for _, path := range []string{
			filepath.Join(root, "sdk", "examples", string(typ), "plugin.yaml"),
			filepath.Join(root, "sdk", "manifests", string(typ)+".plugin.yaml"),
		} {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Errorf("%s: %v", path, err)
				continue
			}
			m, err := Parse(data)
			if err != nil {
				t.Errorf("%s: %v", path, err)
				continue
			}
			if m.Spec.Type != string(typ) {
				t.Errorf("%s declares type %s", path, m.Spec.Type)
			}
			if _, err := m.ResolveConfig(nil); err != nil {
				t.Errorf("%s: defaults do not resolve: %v", path, err)
			}
		}
	}
}

// The JSON Schema shipped for editors must at least be valid JSON, and its list of
// plugin types must match the platform's.
func TestSDKJSONSchemaMatchesTheTypeList(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(testutil.RepoRoot(), "sdk", "manifests", "plugin.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Properties struct {
			Spec struct {
				Properties struct {
					Type struct {
						Enum []string `json:"enum"`
					} `json:"type"`
				} `json:"properties"`
			} `json:"spec"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatalf("plugin.schema.json is not valid JSON: %v", err)
	}
	have := map[string]bool{}
	for _, e := range schema.Properties.Spec.Properties.Type.Enum {
		have[e] = true
	}
	for _, typ := range plugins.AllTypes {
		if !have[string(typ)] {
			t.Errorf("schema lacks type %s", typ)
		}
	}
	if len(have) != len(plugins.AllTypes) {
		t.Errorf("schema lists %d types, the platform has %d", len(have), len(plugins.AllTypes))
	}
}
