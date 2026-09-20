package llm

import (
	"context"
	"log"
)

// fallbackBackend wraps a primary Backend with a secondary one. If the
// primary fails for any reason other than context cancellation/deadline, the
// fallback is tried instead -- so a caller always gets a reply as long as at
// least one of the two backends is working. It never masks context
// cancellation as if all were well: a canceled/expired ctx is returned
// immediately without trying the fallback, since a second network call is
// pointless once the client has already given up.
//
// This is a generic decorator, not a registered backend of its own -- it's
// composed by New() when LLM_FALLBACK_MODE is set, so any two registered
// backends (built-in or a drop-in plugin) can be paired without a new named
// combination for every pair.
type fallbackBackend struct {
	primary  Backend
	fallback Backend
}

var _ Backend = (*fallbackBackend)(nil)

// NewFallback wraps primary with fallback: fallback is only tried if primary
// fails and the request's context is still live.
func NewFallback(primary, fallback Backend) Backend {
	return &fallbackBackend{primary: primary, fallback: fallback}
}

// Usage reports the primary backend's usage, since that is the metered one.
func (b *fallbackBackend) Usage() (UsageSnapshot, bool) {
	if r, ok := b.primary.(UsageReporter); ok {
		return r.Usage()
	}
	return UsageSnapshot{}, false
}

func (b *fallbackBackend) Complete(ctx context.Context, prompt string) (string, error) {
	reply, err := b.primary.Complete(ctx, prompt)
	if err == nil {
		return reply, nil
	}
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	log.Printf("llm fallback used: primary_err=%v", err)
	return b.fallback.Complete(ctx, prompt)
}
