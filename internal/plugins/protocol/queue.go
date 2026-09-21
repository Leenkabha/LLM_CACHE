package protocol

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"time"

	"github.com/leenkabha/llm_cache/internal/cachequeue"
)

// Queue contract (v1), lease based, because a Go callback cannot cross a process
// boundary:
//
//	POST /v1/jobs                  {"prompt","reply","vector":[..]}  -> 201 {"id":"job-id"}   (durable, or an error)
//	POST /v1/jobs/lease            {"max_jobs":1,"visibility_timeout_seconds":60,"wait_seconds":5}
//	                               -> 200 {"jobs":[{"id","prompt","reply","vector","lease_token","attempt"}]} | 204 no job
//	POST /v1/jobs/{id}/ack         {"lease_token":"t"}               -> 200 | 409 lease lost
//	POST /v1/jobs/{id}/nack        {"lease_token":"t"}               -> 200 | 409 lease lost   (job becomes visible again)
//	GET  /v1/jobs/stats (optional) -> {"pending":N,"leased":M}
//	GET  /health
//
// A leased job is invisible to other consumers until the visibility timeout
// passes; if it is neither acked nor nacked it is redelivered. Acknowledging is
// therefore the ONLY thing that removes a job, and the adapter acks only after
// the cache-update handler has returned success.

// QueueOptions tunes the worker loop.
type QueueOptions struct {
	VisibilityTimeout time.Duration // default 60s
	LeaseWait         time.Duration // server-side long-poll wait, default 2s
	IdleBackoff       time.Duration // client-side pause after an empty/failed lease, default 500ms
}

// Queue is the remote adapter for cachequeue.Queue.
type Queue struct {
	c    *Client
	opts QueueOptions
}

var (
	_ cachequeue.Queue   = (*Queue)(nil)
	_ cachequeue.Depther = (*Queue)(nil)
)

func NewQueue(c *Client, opts QueueOptions) *Queue {
	if opts.VisibilityTimeout <= 0 {
		opts.VisibilityTimeout = 60 * time.Second
	}
	if opts.LeaseWait <= 0 {
		opts.LeaseWait = 2 * time.Second
	}
	if opts.IdleBackoff <= 0 {
		opts.IdleBackoff = 500 * time.Millisecond
	}
	cc := *c
	if cc.MaxResponse == 0 || cc.MaxResponse > 16<<20 {
		cc.MaxResponse = 16 << 20
	}
	// The lease call long-polls, so its HTTP timeout must exceed the wait.
	if cc.Timeout < opts.LeaseWait+5*time.Second {
		cc.Timeout = opts.LeaseWait + 5*time.Second
	}
	return &Queue{c: &cc, opts: opts}
}

type qJob struct {
	Prompt string    `json:"prompt"`
	Reply  string    `json:"reply"`
	Vector []float64 `json:"vector"`
}

// Enqueue appends a job. It returns an error unless the plugin confirms the job
// was stored durably (201 with an id).
func (q *Queue) Enqueue(ctx context.Context, job cachequeue.Job) error {
	var out struct {
		ID string `json:"id"`
	}
	status, err := q.c.Do(ctx, http.MethodPost, "/v1/jobs", qJob{job.Prompt, job.Reply, job.Vector}, &out)
	if err != nil {
		return fmt.Errorf("queue plugin: enqueue: %w", err)
	}
	if status != http.StatusCreated || !ValidID(out.ID) {
		return errors.New("queue plugin did not confirm a durable enqueue")
	}
	return nil
}

type leasedJob struct {
	ID         string    `json:"id"`
	Prompt     string    `json:"prompt"`
	Reply      string    `json:"reply"`
	Vector     []float64 `json:"vector"`
	LeaseToken string    `json:"lease_token"`
	Attempt    int       `json:"attempt"`
}

type leaseResponse struct {
	Jobs []leasedJob `json:"jobs"`
}

// lease asks for one job. It returns nil, nil when there is none.
func (q *Queue) lease(ctx context.Context) (*leasedJob, error) {
	req := map[string]any{
		"max_jobs":                   1,
		"visibility_timeout_seconds": int(q.opts.VisibilityTimeout / time.Second),
		"wait_seconds":               int(q.opts.LeaseWait / time.Second),
	}
	var out leaseResponse
	status, err := q.c.Do(ctx, http.MethodPost, "/v1/jobs/lease", req, &out)
	if err != nil {
		return nil, err
	}
	if status == http.StatusNoContent || len(out.Jobs) == 0 {
		return nil, nil
	}
	if len(out.Jobs) > 1 {
		return nil, errors.New("queue plugin returned more jobs than requested")
	}
	j := out.Jobs[0]
	if !ValidID(j.ID) || j.LeaseToken == "" {
		return nil, errors.New("queue plugin returned a job without a valid id and lease token")
	}
	return &j, nil
}

// settle sends ack or nack on a context that survives worker cancellation.
func (q *Queue) settle(action string, j *leasedJob) error {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, err := q.c.Do(ctx, http.MethodPost, "/v1/jobs/"+url.PathEscape(j.ID)+"/"+action,
			map[string]string{"lease_token": j.LeaseToken}, nil)
		cancel()
		if err == nil {
			return nil
		}
		var se *StatusError
		if errors.As(err, &se) && se.Status == http.StatusConflict {
			return err // lease lost: retrying cannot help
		}
		lastErr = err
		time.Sleep(time.Duration(attempt+1) * 100 * time.Millisecond)
	}
	return lastErr
}

// Run leases jobs until ctx is canceled. A job is acknowledged only after the
// handler returns nil. If the handler fails (or panics) the job is nacked so it
// becomes visible immediately; if even the nack fails, the visibility timeout
// redelivers it. Run returns only after the in-flight job has been settled.
func (q *Queue) Run(ctx context.Context, handler func(context.Context, cachequeue.Job) error) {
	for ctx.Err() == nil {
		j, err := q.lease(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("queue plugin: lease failed: %v", err)
			sleepCtx(ctx, q.opts.IdleBackoff*4)
			continue
		}
		if j == nil {
			sleepCtx(ctx, q.opts.IdleBackoff)
			continue
		}
		q.process(ctx, j, handler)
	}
}

func (q *Queue) process(ctx context.Context, j *leasedJob, handler func(context.Context, cachequeue.Job) error) {
	var herr error
	func() {
		defer func() {
			if r := recover(); r != nil {
				herr = fmt.Errorf("cache-update handler panicked: %v", r)
			}
		}()
		herr = handler(ctx, cachequeue.Job{Prompt: j.Prompt, Reply: j.Reply, Vector: j.Vector})
	}()
	if herr != nil {
		log.Printf("queue plugin: job %s failed (attempt %d): %v", j.ID, j.Attempt, herr)
		if err := q.settle("nack", j); err != nil {
			log.Printf("queue plugin: nack of job %s failed, relying on visibility timeout: %v", j.ID, err)
		}
		return
	}
	if err := q.settle("ack", j); err != nil {
		// The handler succeeded but the ack did not land: the job will be
		// redelivered after the visibility timeout (at-least-once delivery).
		log.Printf("queue plugin: ack of job %s failed, it may be redelivered: %v", j.ID, err)
	}
}

func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// Depth implements cachequeue.Depther via the optional stats endpoint.
func (q *Queue) Depth(ctx context.Context) (int, error) {
	var out struct {
		Pending *int `json:"pending"`
		Leased  int  `json:"leased"`
	}
	if _, err := q.c.Do(ctx, http.MethodGet, "/v1/jobs/stats", nil, &out); err != nil {
		return 0, err
	}
	if out.Pending == nil {
		return 0, ErrMalformed
	}
	return *out.Pending + out.Leased, nil
}

// Health checks GET /health.
func (q *Queue) Health(ctx context.Context) error { return q.c.Health(ctx) }
