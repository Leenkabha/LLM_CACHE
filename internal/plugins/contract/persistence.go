package contract

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/leenkabha/llm_cache/internal/persistence"
	"github.com/leenkabha/llm_cache/internal/plugins/protocol"
)

func runPersistence(r *runner, t Target) {
	p := protocol.NewPersistence(t.Client)
	prefix := fmt.Sprintf("contract-%d-", time.Now().UnixNano()%1_000_000_000)
	id := func(s string) string { return prefix + s }
	now := time.Now().UTC().Truncate(time.Millisecond)
	entry := func(s string, n int) persistence.Entry {
		return persistence.Entry{ID: id(s), Prompt: "prompt " + s, Reply: "reply " + s, Vector: []float64{float64(n), 0.5, -0.25}, CreatedAt: now.Add(time.Duration(n) * time.Second)}
	}
	startedEmpty := false
	var created []string
	cleanup := func() {
		for _, c := range created {
			_ = p.Delete(c)
		}
	}

	r.check("health", func(ctx context.Context) error { return p.Health() })
	r.check("size_at_start", func(ctx context.Context) error {
		n, err := p.Size()
		if err != nil {
			return err
		}
		startedEmpty = n == 0
		if t.RequireEmpty && n != 0 {
			return fmt.Errorf("store holds %d entries; a candidate persistence store must start empty", n)
		}
		return nil
	})
	r.check("save_load_roundtrip_all_fields", func(ctx context.Context) error {
		e := entry("a", 1)
		created = append(created, e.ID)
		if err := p.Save(e); err != nil {
			return err
		}
		got, err := p.LoadDetailed(e.ID)
		if err != nil {
			return err
		}
		if got.ID != e.ID || got.Prompt != e.Prompt || got.Reply != e.Reply || !reflect.DeepEqual(got.Vector, e.Vector) || !got.CreatedAt.Equal(e.CreatedAt) {
			return fmt.Errorf("entry did not round-trip: saved %+v loaded %+v (every field is needed to rebuild the vector store and policy)", e, got)
		}
		return nil
	})
	r.check("overwrite_updates_entry", func(ctx context.Context) error {
		e := entry("a", 1)
		e.Reply = "changed"
		if err := p.Save(e); err != nil {
			return err
		}
		got, err := p.LoadDetailed(e.ID)
		if err != nil || got.Reply != "changed" {
			return fmt.Errorf("overwrite not visible: %+v %v", got, err)
		}
		return nil
	})
	r.check("not_found_is_distinct_from_failure", func(ctx context.Context) error {
		_, err := p.LoadDetailed(id("missing"))
		if !errors.Is(err, persistence.ErrNotFound) {
			return fmt.Errorf("missing entry gave %v, want ErrNotFound (a 404 not_found)", err)
		}
		return nil
	})
	r.check("unicode_and_large_entries", func(ctx context.Context) error {
		e := entry("big", 2)
		e.Prompt = "héllo 你好 🙂"
		e.Reply = strings.Repeat("r", 256<<10)
		e.Vector = make([]float64, 384)
		for i := range e.Vector {
			e.Vector[i] = float64(i) / 1000
		}
		created = append(created, e.ID)
		if err := p.Save(e); err != nil {
			return err
		}
		got, err := p.LoadDetailed(e.ID)
		if err != nil || got.Prompt != e.Prompt || got.Reply != e.Reply || len(got.Vector) != 384 {
			return fmt.Errorf("large/unicode entry did not round-trip: %v", err)
		}
		return nil
	})
	r.check("url_safe_id_characters", func(ctx context.Context) error {
		e := entry("x", 3)
		e.ID = id("a.b_c~d-e")
		created = append(created, e.ID)
		if err := p.Save(e); err != nil {
			return err
		}
		_, err := p.LoadDetailed(e.ID)
		return err
	})
	r.check("list_and_size", func(ctx context.Context) error {
		before, err := p.Size()
		if err != nil {
			return err
		}
		for i := 0; i < 5; i++ {
			e := entry("l"+strconv.Itoa(i), 10+i)
			created = append(created, e.ID)
			if err := p.Save(e); err != nil {
				return err
			}
		}
		after, err := p.Size()
		if err != nil {
			return err
		}
		if after != before+5 {
			return fmt.Errorf("size went from %d to %d after 5 new entries", before, after)
		}
		all, err := p.List()
		if err != nil {
			return err
		}
		if len(all) != after {
			return fmt.Errorf("List returned %d entries but Size says %d", len(all), after)
		}
		return nil
	})
	r.check("list_pagination", func(ctx context.Context) error {
		var got, pages int
		cursor := ""
		seen := map[string]bool{}
		for {
			path := "/v1/entries?limit=2"
			if cursor != "" {
				path += "&cursor=" + url.QueryEscape(cursor)
			}
			var out struct {
				Entries    []persistence.Entry `json:"entries"`
				NextCursor string              `json:"next_cursor"`
			}
			if _, err := t.Client.Do(ctx, http.MethodGet, path, nil, &out); err != nil {
				return err
			}
			if len(out.Entries) > 2 {
				return fmt.Errorf("limit=2 returned %d entries", len(out.Entries))
			}
			for _, e := range out.Entries {
				if seen[e.ID] {
					return fmt.Errorf("entry %s returned on two pages", e.ID)
				}
				seen[e.ID] = true
				got++
			}
			pages++
			if out.NextCursor == "" {
				break
			}
			if out.NextCursor == cursor || pages > 100000 {
				return errors.New("pagination cursor does not advance")
			}
			cursor = out.NextCursor
		}
		size, _ := p.Size()
		if got != size {
			return fmt.Errorf("pagination visited %d entries, size is %d", got, size)
		}
		return nil
	})
	r.check("delete_is_idempotent", func(ctx context.Context) error {
		e := entry("del", 20)
		if err := p.Save(e); err != nil {
			return err
		}
		if err := p.Delete(e.ID); err != nil {
			return err
		}
		if err := p.Delete(e.ID); err != nil {
			return fmt.Errorf("deleting an unknown id must not fail: %w", err)
		}
		if _, err := p.LoadDetailed(e.ID); !errors.Is(err, persistence.ErrNotFound) {
			return fmt.Errorf("deleted entry still loads: %v", err)
		}
		return nil
	})
	r.check("mismatched_id_rejected", func(ctx context.Context) error {
		e := entry("m", 30)
		return expect4xx(ctx, t.Client, http.MethodPut, "/v1/entries/"+url.PathEscape(id("other")), e)
	})
	r.check("concurrent_saves", func(ctx context.Context) error {
		before, _ := p.Size()
		const n = 16
		ids := make([]string, n)
		for i := range ids {
			ids[i] = id("c" + strconv.Itoa(i))
			created = append(created, ids[i])
		}
		if err := parallel(n, func(i int) error {
			e := entry("c"+strconv.Itoa(i), 40+i)
			if err := p.Save(e); err != nil {
				return err
			}
			_, err := p.LoadDetailed(e.ID)
			return err
		}); err != nil {
			return err
		}
		if after, _ := p.Size(); after != before+n {
			return fmt.Errorf("size %d -> %d after %d concurrent saves", before, after, n)
		}
		return nil
	})
	r.check("flush_clears_store", func(ctx context.Context) error {
		cleanup()
		created = nil
		n, _ := p.Size()
		if n != 0 && !startedEmpty {
			return errSkip("store held other entries at the start; not flushing them")
		}
		e := entry("f", 50)
		if err := p.Save(e); err != nil {
			return err
		}
		if err := p.Flush(); err != nil {
			return err
		}
		if n, err := p.Size(); err != nil || n != 0 {
			return fmt.Errorf("size after flush = %d (%v), want 0", n, err)
		}
		if err := p.Save(entry("after", 51)); err != nil {
			return fmt.Errorf("store unusable after flush: %w", err)
		}
		return p.Flush()
	})
	cleanup()
}
