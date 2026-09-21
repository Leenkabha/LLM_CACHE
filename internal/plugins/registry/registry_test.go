package registry

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/leenkabha/llm_cache/internal/plugins"
	"github.com/leenkabha/llm_cache/internal/plugins/secrets"
)

func stores(t *testing.T) map[string]Store {
	out := map[string]Store{"memory": NewMemory()}
	if addr := os.Getenv("REDIS_TEST_ADDR"); addr != "" {
		r, err := NewRedis(addr)
		if err != nil {
			t.Fatalf("redis: %v", err)
		}
		out["redis"] = r
	}
	return out
}

func rec(id string) *Record {
	return &Record{ID: id, Name: "n-" + id, Version: "1.0.0", Type: plugins.TypeLLM, State: plugins.StateDraft,
		CreatedAt: time.Now().UTC(), Secrets: map[string]secrets.Sealed{"K": {Version: 1, Ciphertext: "x"}}}
}

func TestStoreCRUD(t *testing.T) {
	for name, s := range stores(t) {
		t.Run(name, func(t *testing.T) {
			id := fmt.Sprintf("crud-%d", time.Now().UnixNano())
			if _, err := s.Get(id); !errors.Is(err, ErrNotFound) {
				t.Fatalf("Get missing: %v", err)
			}
			r := rec(id)
			if err := s.Create(r); err != nil {
				t.Fatal(err)
			}
			if err := s.Create(rec(id)); !errors.Is(err, ErrExists) {
				t.Fatalf("duplicate create: %v", err)
			}
			got, err := s.Get(id)
			if err != nil || got.Rev != 1 || got.Secrets["K"].Ciphertext != "x" {
				t.Fatalf("Get = %+v, %v", got, err)
			}
			// Mutating a returned record must not change what is stored.
			got.Name = "mutated"
			again, _ := s.Get(id)
			if again.Name == "mutated" {
				t.Fatal("store aliases returned records")
			}
			upd, err := s.Update(id, func(r *Record) error { r.State = plugins.StateActive; return nil })
			if err != nil || upd.State != plugins.StateActive || upd.Rev != 2 {
				t.Fatalf("Update = %+v, %v", upd, err)
			}
			boom := errors.New("boom")
			if _, err := s.Update(id, func(r *Record) error { r.State = plugins.StateFailed; return boom }); !errors.Is(err, boom) {
				t.Fatalf("Update error = %v", err)
			}
			cur, _ := s.Get(id)
			if cur.State != plugins.StateActive {
				t.Fatal("a failed update was written")
			}
			if _, err := s.Update("nope"+id, func(*Record) error { return nil }); !errors.Is(err, ErrNotFound) {
				t.Fatalf("Update missing: %v", err)
			}
			if err := s.Delete(id); err != nil {
				t.Fatal(err)
			}
			if err := s.Delete(id); !errors.Is(err, ErrNotFound) {
				t.Fatalf("second delete: %v", err)
			}
		})
	}
}

func TestStoreConcurrentUpdatesAreNotLost(t *testing.T) {
	for name, s := range stores(t) {
		t.Run(name, func(t *testing.T) {
			id := fmt.Sprintf("conc-%d", time.Now().UnixNano())
			r := rec(id)
			r.Config = map[string]any{"n": float64(0)}
			if err := s.Create(r); err != nil {
				t.Fatal(err)
			}
			var wg sync.WaitGroup
			const workers = 20
			for i := 0; i < workers; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					if _, err := s.Update(id, func(r *Record) error { r.Config["n"] = r.Config["n"].(float64) + 1; return nil }); err != nil {
						t.Error(err)
					}
				}()
			}
			wg.Wait()
			got, _ := s.Get(id)
			if got.Config["n"].(float64) != workers {
				t.Fatalf("lost updates: n = %v, want %d", got.Config["n"], workers)
			}
			_ = s.Delete(id)
		})
	}
}

func TestActiveAuditLogs(t *testing.T) {
	for name, s := range stores(t) {
		t.Run(name, func(t *testing.T) {
			if err := s.SetActive(plugins.TypeQueue, "abc"); err != nil {
				t.Fatal(err)
			}
			a, _ := s.Active()
			if a[plugins.TypeQueue] != "abc" {
				t.Fatalf("active = %v", a)
			}
			if err := s.ClearActive(plugins.TypeQueue); err != nil {
				t.Fatal(err)
			}
			a, _ = s.Active()
			if _, ok := a[plugins.TypeQueue]; ok {
				t.Fatal("ClearActive left the pointer")
			}

			pid := fmt.Sprintf("aud-%d", time.Now().UnixNano())
			for i := 0; i < 3; i++ {
				_ = s.AppendAudit(Event{Time: time.Now(), Action: fmt.Sprintf("a%d", i), PluginID: pid, Outcome: "ok"})
			}
			ev, _ := s.Audit(pid, 2)
			if len(ev) != 2 || ev[0].Action != "a2" {
				t.Fatalf("audit newest-first limit: %+v", ev)
			}

			for i := 0; i < 5; i++ {
				_ = s.AppendLog(pid, fmt.Sprintf("line %d", i))
			}
			logs, _ := s.Logs(pid, 3)
			if len(logs) != 3 || logs[2] != "line 4" {
				t.Fatalf("logs tail: %v", logs)
			}
		})
	}
}
