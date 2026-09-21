package orchestrator

import (
	"log"
	"strings"
	"time"
)

// isTransient reports whether a startup error is a dependency that is not up yet
// (a connection refused, a name that does not resolve yet, a timeout) rather than
// a configuration mistake such as an unknown backend name.
func isTransient(err error) bool {
	msg := strings.ToLower(err.Error())
	for _, s := range []string{"connection refused", "no such host", "i/o timeout", "ping failed", "unreachable", "connection reset", "eof"} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

// RetryTransient runs fn, retrying every interval for up to wait while it fails
// with a transient dependency error. A whole-stack restart starts every container
// at once, so the orchestrator may come up before Redis or the plugin controller
// is listening; failing fast there leaves the stack down. Configuration errors
// still fail immediately.
func RetryTransient(what string, wait, interval time.Duration, fn func() error) error {
	deadline := time.Now().Add(wait)
	for {
		err := fn()
		if err == nil || !isTransient(err) || time.Now().After(deadline) {
			return err
		}
		log.Printf("startup: %s not ready (%v); retrying in %s", what, err, interval)
		time.Sleep(interval)
	}
}
