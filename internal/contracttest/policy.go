// Package contracttest provides reusable adapter contract tests. It uses
// structural interfaces to avoid import cycles with adapter packages.
// Import it from _test.go files only.
package contracttest

import (
	"fmt"
	"sync"
	"testing"
)

type Policy interface {
	Name() string
	OnHit(string)
	OnInsert(string)
	OnDelete(string)
	Victim() (string, bool)
	Flush()
}

// PolicyContract checks lifecycle and concurrency without prescribing which
// live entry an algorithm should evict. The factory must return fresh state.
// Add algorithm-specific tests separately. Run with -race to detect data races.
func PolicyContract(t *testing.T, factory func() Policy) {
	t.Helper()
	t.Run("lifecycle", func(t *testing.T) {
		p := factory()
		name := p.Name()
		if name == "" {
			t.Fatal("Name must not be empty")
		}
		if _, ok := p.Victim(); ok {
			t.Fatal("empty policy returned a victim")
		}
		p.OnHit("absent")
		p.OnDelete("absent")
		if _, ok := p.Victim(); ok {
			t.Fatal("unknown IDs must not create entries")
		}
		for _, id := range []string{"a", "b", "c"} {
			p.OnInsert(id)
		}
		p.OnInsert("a")
		p.OnHit("b")
		remaining := map[string]bool{"a": true, "b": true, "c": true}
		for len(remaining) > 0 {
			id, ok := p.Victim()
			if !ok || !remaining[id] {
				t.Fatalf("victim %q is not a live entry", id)
			}
			// Asking again must not consume a candidate.
			again, ok := p.Victim()
			if !ok || !remaining[again] {
				t.Fatal("Victim consumed or returned an unknown entry")
			}
			p.OnDelete(id)
			p.OnDelete(id)
			delete(remaining, id)
		}
		if _, ok := p.Victim(); ok {
			t.Fatal("duplicate insertion or selection left stale state")
		}
		p.OnInsert("reuse")
		p.Flush()
		p.Flush()
		if _, ok := p.Victim(); ok {
			t.Fatal("Flush left entries")
		}
		p.OnInsert("new")
		if id, ok := p.Victim(); !ok || id != "new" {
			t.Fatal("policy cannot be reused after Flush")
		}
		if p.Name() != name {
			t.Fatal("Name changed during lifecycle")
		}
	})
	t.Run("concurrent_hooks", func(t *testing.T) {
		p := factory()
		var wg sync.WaitGroup
		for worker := 0; worker < 8; worker++ {
			wg.Add(1)
			go func(worker int) {
				defer wg.Done()
				for n := 0; n < 50; n++ {
					id := fmt.Sprintf("%d-%d", worker, n)
					p.OnInsert(id)
					p.OnHit(id)
					p.Victim()
					p.Name()
					p.OnDelete(id)
				}
			}(worker)
		}
		wg.Wait()
		if _, ok := p.Victim(); ok {
			t.Fatal("concurrent hooks left deleted entries")
		}
	})
}
