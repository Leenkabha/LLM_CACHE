package contract

import (
	"context"
	"errors"
	"fmt"

	"github.com/leenkabha/llm_cache/internal/plugins/protocol"
)

func runPolicy(r *runner, t Target) {
	var p interface {
		Name() string
		OnHit(string)
		OnInsert(string)
		OnDelete(string)
		Victim() (string, bool)
		Flush()
	}
	var closer func()
	r.check("health_and_name", func(ctx context.Context) error {
		pp, err := protocol.NewPolicy(ctx, t.Client)
		if err != nil {
			return err
		}
		p, closer = pp, pp.Close
		if err := pp.Health(ctx); err != nil {
			return err
		}
		if p.Name() == "" {
			return errors.New("empty policy name")
		}
		return nil
	})
	if p == nil {
		return
	}
	defer closer()

	r.check("lifecycle", func(ctx context.Context) error {
		p.Flush()
		if _, ok := p.Victim(); ok {
			return errors.New("empty policy returned a victim")
		}
		p.OnHit("absent")
		p.OnDelete("absent")
		if _, ok := p.Victim(); ok {
			return errors.New("hit/delete of unknown ids created entries")
		}
		for _, id := range []string{"a", "b", "c"} {
			p.OnInsert(id)
		}
		p.OnInsert("a") // repeated insertion must not duplicate state
		p.OnHit("b")
		remaining := map[string]bool{"a": true, "b": true, "c": true}
		for len(remaining) > 0 {
			id, ok := p.Victim()
			if !ok || !remaining[id] {
				return fmt.Errorf("victim %q (ok=%v) is not a live entry %v", id, ok, remaining)
			}
			again, ok := p.Victim()
			if !ok || !remaining[again] {
				return errors.New("Victim consumed a candidate or returned an unknown entry on the second call")
			}
			p.OnDelete(id)
			p.OnDelete(id) // repeated deletion is harmless
			delete(remaining, id)
		}
		if _, ok := p.Victim(); ok {
			return errors.New("duplicate insertion or repeated deletion left stale state")
		}
		return nil
	})
	r.check("flush_leaves_policy_reusable", func(ctx context.Context) error {
		p.OnInsert("reuse")
		p.Flush()
		p.Flush()
		if _, ok := p.Victim(); ok {
			return errors.New("Flush left entries behind")
		}
		p.OnInsert("new")
		if id, ok := p.Victim(); !ok || id != "new" {
			return fmt.Errorf("policy unusable after Flush: victim=%q ok=%v", id, ok)
		}
		p.Flush()
		return nil
	})
	r.check("victim_does_not_delete", func(ctx context.Context) error {
		p.Flush()
		p.OnInsert("only")
		for i := 0; i < 3; i++ {
			if id, ok := p.Victim(); !ok || id != "only" {
				return fmt.Errorf("call %d: victim=%q ok=%v; Victim must select without removing", i+1, id, ok)
			}
		}
		p.Flush()
		return nil
	})
	r.check("concurrent_hooks", func(ctx context.Context) error {
		p.Flush()
		if err := parallel(8, func(w int) error {
			for n := 0; n < 30; n++ {
				id := fmt.Sprintf("%d-%d", w, n)
				p.OnInsert(id)
				p.OnHit(id)
				p.Victim()
				p.Name()
				p.OnDelete(id)
			}
			return nil
		}); err != nil {
			return err
		}
		if id, ok := p.Victim(); ok {
			return fmt.Errorf("concurrent hooks left a deleted entry behind (%q)", id)
		}
		return nil
	})
	r.check("no_events_lost", func(ctx context.Context) error {
		if pp, ok := p.(*protocol.Policy); ok {
			return pp.Health(ctx)
		}
		return nil
	})
}
