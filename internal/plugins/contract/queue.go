package contract

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/leenkabha/llm_cache/internal/cachequeue"
	"github.com/leenkabha/llm_cache/internal/plugins/protocol"
)

type rawJob struct {
	ID         string    `json:"id"`
	Prompt     string    `json:"prompt"`
	Reply      string    `json:"reply"`
	Vector     []float64 `json:"vector"`
	LeaseToken string    `json:"lease_token"`
	Attempt    int       `json:"attempt"`
}

func rawEnqueue(ctx context.Context, c *protocol.Client, prompt string) (string, error) {
	var out struct {
		ID string `json:"id"`
	}
	status, err := c.Do(ctx, http.MethodPost, "/v1/jobs", map[string]any{"prompt": prompt, "reply": "r", "vector": []float64{1, 0}}, &out)
	if err != nil {
		return "", err
	}
	if status != http.StatusCreated || out.ID == "" {
		return "", fmt.Errorf("enqueue answered HTTP %d with id %q, want 201 and an id (durable enqueue)", status, out.ID)
	}
	return out.ID, nil
}

func rawLease(ctx context.Context, c *protocol.Client, visSeconds int) (*rawJob, error) {
	var out struct {
		Jobs []rawJob `json:"jobs"`
	}
	status, err := c.Do(ctx, http.MethodPost, "/v1/jobs/lease", map[string]any{"max_jobs": 1, "visibility_timeout_seconds": visSeconds, "wait_seconds": 0}, &out)
	if err != nil {
		return nil, err
	}
	if status == http.StatusNoContent || len(out.Jobs) == 0 {
		return nil, nil
	}
	return &out.Jobs[0], nil
}

func rawSettle(ctx context.Context, c *protocol.Client, id, action, token string) (int, error) {
	return c.Do(ctx, http.MethodPost, "/v1/jobs/"+id+"/"+action, map[string]string{"lease_token": token}, nil)
}

func runQueue(r *runner, t Target) {
	c := t.Client
	q := protocol.NewQueue(c, protocol.QueueOptions{VisibilityTimeout: 30 * time.Second, LeaseWait: time.Second, IdleBackoff: 50 * time.Millisecond})

	r.check("health", func(ctx context.Context) error { return q.Health(ctx) })
	r.check("queue_starts_empty", func(ctx context.Context) error {
		j, err := rawLease(ctx, c, 1)
		if err != nil {
			return err
		}
		if j != nil {
			_, _ = rawSettle(ctx, c, j.ID, "nack", j.LeaseToken)
			return errors.New("the candidate queue already holds jobs; a candidate queue must start empty")
		}
		return nil
	})
	r.check("enqueue_is_durable_and_lease_returns_payload", func(ctx context.Context) error {
		id, err := rawEnqueue(ctx, c, "p-lease")
		if err != nil {
			return err
		}
		j, err := rawLease(ctx, c, 30)
		if err != nil || j == nil {
			return fmt.Errorf("lease returned %v, %v; the enqueued job should be visible", j, err)
		}
		if j.ID != id || j.Prompt != "p-lease" || j.LeaseToken == "" || len(j.Vector) != 2 {
			return fmt.Errorf("leased job does not match the enqueued one: %+v", j)
		}
		if again, _ := rawLease(ctx, c, 30); again != nil {
			return errors.New("a leased job was handed out again before its visibility timeout")
		}
		if code, err := rawSettle(ctx, c, id, "ack", j.LeaseToken); err != nil {
			return fmt.Errorf("ack failed (%d): %v", code, err)
		}
		if after, _ := rawLease(ctx, c, 30); after != nil {
			return errors.New("an acknowledged job was delivered again")
		}
		return nil
	})
	r.check("visibility_timeout_redelivers_and_stale_ack_rejected", func(ctx context.Context) error {
		id, err := rawEnqueue(ctx, c, "p-vis")
		if err != nil {
			return err
		}
		first, err := rawLease(ctx, c, 1)
		if err != nil || first == nil {
			return fmt.Errorf("first lease: %v %v", first, err)
		}
		time.Sleep(1300 * time.Millisecond)
		second, err := rawLease(ctx, c, 30)
		if err != nil || second == nil || second.ID != id {
			return fmt.Errorf("job was not redelivered after the visibility timeout: %+v %v", second, err)
		}
		if second.LeaseToken == first.LeaseToken {
			return errors.New("redelivery reused the old lease token")
		}
		if second.Attempt <= first.Attempt {
			return fmt.Errorf("attempt did not increase on redelivery (%d -> %d)", first.Attempt, second.Attempt)
		}
		if code, _ := rawSettle(ctx, c, id, "ack", first.LeaseToken); code != http.StatusConflict {
			return fmt.Errorf("ack with a stale lease token answered %d, want 409", code)
		}
		if code, err := rawSettle(ctx, c, id, "ack", second.LeaseToken); err != nil {
			return fmt.Errorf("ack with the current token failed (%d): %v", code, err)
		}
		return nil
	})
	r.check("nack_makes_job_visible_immediately", func(ctx context.Context) error {
		id, err := rawEnqueue(ctx, c, "p-nack")
		if err != nil {
			return err
		}
		j, _ := rawLease(ctx, c, 60)
		if j == nil {
			return errors.New("no job to lease")
		}
		if code, err := rawSettle(ctx, c, id, "nack", j.LeaseToken); err != nil {
			return fmt.Errorf("nack failed (%d): %v", code, err)
		}
		again, _ := rawLease(ctx, c, 60)
		if again == nil || again.ID != id {
			return errors.New("a nacked job was not immediately visible again")
		}
		_, _ = rawSettle(ctx, c, id, "ack", again.LeaseToken)
		return nil
	})
	r.check("wrong_token_and_unknown_job", func(ctx context.Context) error {
		id, err := rawEnqueue(ctx, c, "p-wrong")
		if err != nil {
			return err
		}
		j, _ := rawLease(ctx, c, 60)
		if j == nil {
			return errors.New("no job to lease")
		}
		if code, _ := rawSettle(ctx, c, id, "ack", "not-the-token"); code != http.StatusConflict {
			return fmt.Errorf("ack with a wrong token answered %d, want 409", code)
		}
		if code, _ := rawSettle(ctx, c, "no-such-job", "ack", "tok"); code != http.StatusNotFound {
			return fmt.Errorf("ack of an unknown job answered %d, want 404", code)
		}
		_, _ = rawSettle(ctx, c, id, "ack", j.LeaseToken)
		return nil
	})

	r.check("adapter_acks_only_after_handler_success", func(ctx context.Context) error {
		if _, err := rawEnqueue(ctx, c, "p-adapter"); err != nil {
			return err
		}
		var calls atomic.Int32
		inFlightVisible := make(chan bool, 1)
		done := make(chan struct{})
		rctx, cancel := context.WithCancel(ctx)
		defer cancel()
		go func() {
			defer close(done)
			q.Run(rctx, func(hctx context.Context, job cachequeue.Job) error {
				n := calls.Add(1)
				if n == 1 {
					// While the handler runs, the job must be leased (invisible) and NOT acked.
					other, _ := rawLease(ctx, c, 30)
					inFlightVisible <- other != nil
					time.Sleep(50 * time.Millisecond)
					return errors.New("simulated cache-update failure")
				}
				cancel()
				return nil
			})
		}()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			return errors.New("worker did not finish; the failed job was probably not redelivered")
		}
		if v := <-inFlightVisible; v {
			return errors.New("a job being handled was visible to another consumer")
		}
		if calls.Load() != 2 {
			return fmt.Errorf("handler ran %d times, want 2 (fail, then retry after nack)", calls.Load())
		}
		if j, _ := rawLease(ctx, c, 30); j != nil {
			return errors.New("job still queued after a successful handler; it was not acknowledged")
		}
		return nil
	})
	r.check("run_stops_promptly_when_idle", func(ctx context.Context) error {
		rctx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() { q.Run(rctx, func(context.Context, cachequeue.Job) error { return nil }); close(done) }()
		time.Sleep(200 * time.Millisecond)
		cancel()
		select {
		case <-done:
			return nil
		case <-time.After(8 * time.Second):
			return errors.New("Run did not return after its context was canceled")
		}
	})
	r.check("graceful_shutdown_finishes_inflight_job", func(ctx context.Context) error {
		if _, err := rawEnqueue(ctx, c, "p-graceful"); err != nil {
			return err
		}
		started, release := make(chan struct{}), make(chan struct{})
		rctx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() {
			q.Run(rctx, func(_ context.Context, _ cachequeue.Job) error {
				close(started)
				<-release
				return nil
			})
			close(done)
		}()
		select {
		case <-started:
		case <-time.After(10 * time.Second):
			cancel()
			return errors.New("job was never delivered to the worker")
		}
		cancel() // shutdown requested while the job is in flight
		select {
		case <-done:
			return errors.New("Run returned while a job was still being handled")
		case <-time.After(300 * time.Millisecond):
		}
		close(release)
		select {
		case <-done:
		case <-time.After(8 * time.Second):
			return errors.New("Run did not return after the in-flight handler finished")
		}
		if j, _ := rawLease(ctx, c, 30); j != nil {
			return errors.New("the in-flight job finished but was not acknowledged before shutdown")
		}
		return nil
	})
	r.check("concurrent_consumers_process_each_job_once", func(ctx context.Context) error {
		const n = 12
		for i := 0; i < n; i++ {
			if _, err := rawEnqueue(ctx, c, fmt.Sprintf("p-conc-%d", i)); err != nil {
				return err
			}
		}
		var mu sync.Mutex
		seen := map[string]int{}
		rctx, cancel := context.WithCancel(ctx)
		defer cancel()
		var wg sync.WaitGroup
		for w := 0; w < 2; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				q.Run(rctx, func(_ context.Context, job cachequeue.Job) error {
					mu.Lock()
					seen[job.Prompt]++
					total := len(seen)
					mu.Unlock()
					if total >= n {
						go func() { time.Sleep(200 * time.Millisecond); cancel() }()
					}
					return nil
				})
			}()
		}
		wait := make(chan struct{})
		go func() { wg.Wait(); close(wait) }()
		select {
		case <-wait:
		case <-time.After(20 * time.Second):
			cancel()
			return errors.New("jobs were not all processed in time")
		}
		mu.Lock()
		defer mu.Unlock()
		if len(seen) != n {
			return fmt.Errorf("%d of %d jobs processed; jobs were lost", len(seen), n)
		}
		for p, c := range seen {
			if c != 1 {
				return fmt.Errorf("job %s processed %d times in a failure-free run", p, c)
			}
		}
		return nil
	})
	r.check("stats_optional", func(ctx context.Context) error {
		var raw map[string]any
		status, err := c.Do(ctx, http.MethodGet, "/v1/jobs/stats", nil, &raw)
		if err != nil && status != http.StatusNotFound {
			return err
		}
		return nil
	})
}
