package orchestrator

import (
	"errors"
	"testing"
	"time"
)

func TestRetryTransientRetriesDependenciesThatAreNotUpYet(t *testing.T) {
	calls := 0
	err := RetryTransient("redis", time.Second, time.Millisecond, func() error {
		calls++
		if calls < 4 {
			return errors.New("build persistence store: redis ping failed: dial tcp 10.0.0.5:6379: connect: connection refused")
		}
		return nil
	})
	if err != nil || calls != 4 {
		t.Fatalf("err=%v calls=%d, want success on the 4th attempt", err, calls)
	}
}

func TestRetryTransientFailsFastOnConfigurationErrors(t *testing.T) {
	calls := 0
	err := RetryTransient("deps", time.Minute, time.Millisecond, func() error {
		calls++
		return errors.New(`unknown llm mode "gemni" (registered: [example-http gemini openai stub])`)
	})
	if err == nil || calls != 1 {
		t.Fatalf("a configuration error must not be retried: err=%v calls=%d", err, calls)
	}
}

func TestRetryTransientGivesUp(t *testing.T) {
	start := time.Now()
	err := RetryTransient("redis", 30*time.Millisecond, 5*time.Millisecond, func() error { return errors.New("connection refused") })
	if err == nil || time.Since(start) > 2*time.Second {
		t.Fatalf("err=%v after %s", err, time.Since(start))
	}
}
