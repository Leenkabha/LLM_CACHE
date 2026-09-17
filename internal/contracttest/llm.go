package contracttest

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type LLM interface {
	Complete(context.Context, string) (string, error)
}

// LLMContract checks a deterministic local test backend. Supply a prompt and
// its expected reply; never point this suite at a billed production provider.
// Provider-specific HTTP errors and in-flight cancellation need adapter tests.
func LLMContract(t *testing.T, factory func() LLM, prompt, want string) {
	t.Helper()
	t.Run("reply", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		got, err := factory().Complete(ctx, prompt)
		if err != nil || got != want {
			t.Fatalf("reply=%q error=%v", got, err)
		}
	})
	t.Run("canceled_context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		reply, err := factory().Complete(ctx, prompt)
		if !errors.Is(err, context.Canceled) || reply != "" {
			t.Fatalf("reply=%q error=%v, want cancellation", reply, err)
		}
	})
	t.Run("expired_deadline", func(t *testing.T) {
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		defer cancel()
		reply, err := factory().Complete(ctx, prompt)
		if !errors.Is(err, context.DeadlineExceeded) || reply != "" {
			t.Fatalf("reply=%q error=%v, want deadline", reply, err)
		}
	})
	t.Run("concurrent_requests", func(t *testing.T) {
		b := factory()
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				if got, err := b.Complete(ctx, prompt); err != nil || got != want {
					t.Errorf("reply=%q error=%v", got, err)
				}
			}()
		}
		wg.Wait()
	})
}
