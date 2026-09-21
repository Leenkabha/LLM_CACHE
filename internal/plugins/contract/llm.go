package contract

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/leenkabha/llm_cache/internal/plugins/protocol"
)

func runLLM(r *runner, t Target) {
	model := t.Model
	if model == "" {
		model = "contract-test"
	}
	b := protocol.NewLLM(t.Client, model)

	r.check("health", func(ctx context.Context) error { return b.Health(ctx) })
	r.check("reply", func(ctx context.Context) error {
		got, err := b.Complete(ctx, "contract test: say hello")
		if err != nil {
			return err
		}
		if strings.TrimSpace(got) == "" {
			return errors.New("empty reply")
		}
		return nil
	})
	r.check("unicode_and_large_prompt", func(ctx context.Context) error {
		if _, err := b.Complete(ctx, "héllo wörld 你好 🙂 "+strings.Repeat("x", 64<<10)); err != nil {
			return fmt.Errorf("64 KiB unicode prompt failed: %w", err)
		}
		return nil
	})
	r.check("canceled_context", func(ctx context.Context) error {
		c, cancel := context.WithCancel(ctx)
		cancel()
		reply, err := b.Complete(c, "hi")
		if !errors.Is(err, context.Canceled) || reply != "" {
			return fmt.Errorf("reply=%q err=%v, want context.Canceled and no reply", reply, err)
		}
		return nil
	})
	r.check("expired_deadline", func(ctx context.Context) error {
		c, cancel := context.WithDeadline(ctx, time.Now().Add(-time.Second))
		defer cancel()
		reply, err := b.Complete(c, "hi")
		if !errors.Is(err, context.DeadlineExceeded) || reply != "" {
			return fmt.Errorf("reply=%q err=%v, want context.DeadlineExceeded and no reply", reply, err)
		}
		return nil
	})
	r.check("malformed_request_rejected", func(ctx context.Context) error {
		return expect4xx(ctx, t.Client, http.MethodPost, "/v1/complete", map[string]any{"model": model, "prompt": ""})
	})
	r.check("concurrent_requests", func(ctx context.Context) error {
		return parallel(8, func(i int) error {
			_, err := b.Complete(ctx, fmt.Sprintf("concurrent %d", i))
			return err
		})
	})
	r.check("usage_optional", func(ctx context.Context) error {
		var raw map[string]any
		status, err := t.Client.Do(ctx, http.MethodGet, "/v1/usage", nil, &raw)
		if err != nil {
			if status == http.StatusNotFound {
				return nil // optional endpoint
			}
			return err
		}
		return nil
	})
}
