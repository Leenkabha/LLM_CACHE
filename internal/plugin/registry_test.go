package plugin

import (
	"errors"
	"strings"
	"testing"

	"github.com/leenkabha/llm_cache/internal/config"
)

type widget struct{ name string }

func TestRegistryBuildValid(t *testing.T) {
	r := NewRegistry[*widget]("widget")
	r.Register("a", func(config.Config) (*widget, error) { return &widget{name: "a"}, nil })

	got, err := r.Build("a", config.Config{})
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if got.name != "a" {
		t.Fatalf("Build() = %+v, want name=a", got)
	}
}

func TestRegistryBuildUnknownListsRegistered(t *testing.T) {
	r := NewRegistry[*widget]("widget")
	r.Register("a", func(config.Config) (*widget, error) { return &widget{name: "a"}, nil })
	r.Register("b", func(config.Config) (*widget, error) { return &widget{name: "b"}, nil })

	_, err := r.Build("missing", config.Config{})
	if err == nil {
		t.Fatal("Build(\"missing\") returned nil error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "widget") || !strings.Contains(msg, "missing") || !strings.Contains(msg, "a") || !strings.Contains(msg, "b") {
		t.Fatalf("error message = %q, want it to name the kind, the requested name, and registered names", msg)
	}
}

func TestRegistryHasAndNames(t *testing.T) {
	r := NewRegistry[*widget]("widget")
	if r.Has("a") {
		t.Fatal("Has(\"a\") = true before registration")
	}
	r.Register("b", func(config.Config) (*widget, error) { return &widget{}, nil })
	r.Register("a", func(config.Config) (*widget, error) { return &widget{}, nil })
	if !r.Has("a") || !r.Has("b") {
		t.Fatal("Has() false for registered names")
	}
	if r.Has("c") {
		t.Fatal("Has(\"c\") = true for unregistered name")
	}
	names := r.Names()
	if len(names) != 2 || names[0] != "a" || names[1] != "b" {
		t.Fatalf("Names() = %v, want sorted [a b]", names)
	}
}

// Register on an existing name replaces the factory -- this documents the
// current last-write-wins behavior rather than rejecting the duplicate.
func TestRegistryDuplicateRegistrationReplaces(t *testing.T) {
	r := NewRegistry[*widget]("widget")
	r.Register("a", func(config.Config) (*widget, error) { return &widget{name: "first"}, nil })
	r.Register("a", func(config.Config) (*widget, error) { return &widget{name: "second"}, nil })

	got, err := r.Build("a", config.Config{})
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if got.name != "second" {
		t.Fatalf("Build() = %+v, want the most recently registered factory to win", got)
	}
	if len(r.Names()) != 1 {
		t.Fatalf("Names() = %v, want a single entry for the duplicated name", r.Names())
	}
}

func TestRegistryFactoryFailurePropagates(t *testing.T) {
	r := NewRegistry[*widget]("widget")
	wantErr := errors.New("boom")
	r.Register("broken", func(config.Config) (*widget, error) { return nil, wantErr })

	_, err := r.Build("broken", config.Config{})
	if !errors.Is(err, wantErr) {
		t.Fatalf("Build() error = %v, want %v", err, wantErr)
	}
}

func TestRegistryFactoryReceivesConfig(t *testing.T) {
	r := NewRegistry[*widget]("widget")
	var received config.Config
	r.Register("a", func(cfg config.Config) (*widget, error) {
		received = cfg
		return &widget{}, nil
	})
	cfg := config.Config{Policy: "lru", Capacity: 42}
	if _, err := r.Build("a", cfg); err != nil {
		t.Fatal(err)
	}
	if received != cfg {
		t.Fatalf("factory received %+v, want %+v", received, cfg)
	}
}

// Two independently constructed registries -- even for the same element type
// -- must not share state. This is what lets every seam (llm, embedder,
// vectorstore, persistence, queue, policy) keep a private package-level
// registry without name collisions across seams.
func TestSeparateRegistriesDoNotInterfere(t *testing.T) {
	r1 := NewRegistry[*widget]("widget-1")
	r2 := NewRegistry[*widget]("widget-2")

	r1.Register("shared-name", func(config.Config) (*widget, error) { return &widget{name: "from-r1"}, nil })
	r2.Register("shared-name", func(config.Config) (*widget, error) { return &widget{name: "from-r2"}, nil })

	got1, err := r1.Build("shared-name", config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	got2, err := r2.Build("shared-name", config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if got1.name != "from-r1" || got2.name != "from-r2" {
		t.Fatalf("registries interfered: r1=%+v r2=%+v", got1, got2)
	}

	// Registering only in r1 must not make the name resolvable in r2.
	r1.Register("only-in-r1", func(config.Config) (*widget, error) { return &widget{}, nil })
	if r2.Has("only-in-r1") {
		t.Fatal("r2.Has(\"only-in-r1\") = true, registries leaked state")
	}
}
