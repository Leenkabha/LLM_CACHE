package llm

import (
	"context"
	"errors"
	"testing"

	"github.com/leenkabha/llm_cache/internal/config"
)

type stringBackend struct {
	reply string
	err   error
	calls int
}

func (b *stringBackend) Complete(ctx context.Context, prompt string) (string, error) {
	b.calls++
	if b.err != nil {
		return "", b.err
	}
	return b.reply, nil
}

func TestFallbackNotUsedWhenPrimarySucceeds(t *testing.T) {
	primary := &stringBackend{reply: "primary reply"}
	fallback := &stringBackend{reply: "fallback reply"}
	b := NewFallback(primary, fallback)

	reply, err := b.Complete(context.Background(), "hello")
	if err != nil || reply != "primary reply" {
		t.Fatalf("reply=%q err=%v", reply, err)
	}
	if fallback.calls != 0 {
		t.Fatalf("fallback.calls=%d, want 0 -- must not be invoked when primary succeeds", fallback.calls)
	}
}

func TestFallbackUsedWhenPrimaryFails(t *testing.T) {
	primary := &stringBackend{err: errors.New("primary down")}
	fallback := &stringBackend{reply: "fallback reply"}
	b := NewFallback(primary, fallback)

	reply, err := b.Complete(context.Background(), "hello")
	if err != nil || reply != "fallback reply" {
		t.Fatalf("reply=%q err=%v", reply, err)
	}
	if primary.calls != 1 || fallback.calls != 1 {
		t.Fatalf("primary.calls=%d fallback.calls=%d, want 1 each", primary.calls, fallback.calls)
	}
}

func TestFallbackReturnsFallbackErrorWhenBothFail(t *testing.T) {
	primary := &stringBackend{err: errors.New("primary down")}
	fallbackErr := errors.New("fallback also down")
	fallback := &stringBackend{err: fallbackErr}
	b := NewFallback(primary, fallback)

	_, err := b.Complete(context.Background(), "hello")
	if !errors.Is(err, fallbackErr) {
		t.Fatalf("err=%v, want %v", err, fallbackErr)
	}
}

func TestFallbackDoesNotMaskContextCancellation(t *testing.T) {
	primary := &stringBackend{err: errors.New("primary down")}
	fallback := &stringBackend{reply: "should not be used"}
	b := NewFallback(primary, fallback)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := b.Complete(ctx, "hello")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v, want context.Canceled", err)
	}
	if fallback.calls != 0 {
		t.Fatal("fallback must not be tried once the caller has already given up")
	}
}

// End-to-end wiring: New() only composes a fallback when LLMFallbackMode is
// set, and leaves single-backend behavior byte-for-byte unchanged otherwise.
func TestNewComposesFallbackOnlyWhenConfigured(t *testing.T) {
	backend, err := New(config.Config{LLMMode: ModeStub})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := backend.(*fallbackBackend); ok {
		t.Fatal("New() wrapped in a fallback even though LLMFallbackMode was empty")
	}

	backend, err = New(config.Config{LLMMode: ModeStub, LLMFallbackMode: ModeStub})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := backend.(*fallbackBackend); !ok {
		t.Fatal("New() did not wrap in a fallback even though LLMFallbackMode was set")
	}
}

func TestNewRejectsUnknownFallbackMode(t *testing.T) {
	_, err := New(config.Config{LLMMode: ModeStub, LLMFallbackMode: "not-a-real-backend"})
	if err == nil {
		t.Fatal("expected an error for an unknown fallback backend")
	}
}
