package policy

import (
	"github.com/leenkabha/llm_cache/internal/config"
	"github.com/leenkabha/llm_cache/internal/contracttest"
	"testing"
)

func TestPolicyContracts(t *testing.T) {
	for _, name := range []string{PolicyLRU, PolicyLFU, PolicyFIFO} {
		t.Run(name, func(t *testing.T) {
			contracttest.PolicyContract(t, func() contracttest.Policy {
				p, err := registry.Build(name, config.Config{})
				if err != nil {
					t.Fatal(err)
				}
				return p
			})
		})
	}
}

func TestFIFOIgnoresHitsAndDuplicateInserts(t *testing.T) {
	p, err := New(config.Config{Policy: PolicyFIFO})
	if err != nil {
		t.Fatal(err)
	}
	p.OnInsert("a")
	p.OnInsert("b")
	p.OnInsert("c")
	p.OnHit("a")
	p.OnInsert("a")
	for _, want := range []string{"a", "b", "c"} {
		got, ok := p.Victim()
		if !ok || got != want {
			t.Fatalf("victim=%q want=%q", got, want)
		}
		p.OnDelete(got)
	}
}

func TestPolicyFactoryReceivesConfiguration(t *testing.T) {
	const name = "test-config-forwarding"
	var received config.Config
	Register(name, func(cfg config.Config) (EvictionPolicy, error) {
		received = cfg
		return registry.Build(PolicyFIFO, cfg)
	})
	cfg := config.Config{Policy: name, Capacity: 17, TopK: 3}
	if _, err := New(cfg); err != nil {
		t.Fatal(err)
	}
	if received != cfg {
		t.Fatalf("factory config=%+v, want %+v", received, cfg)
	}
}
