package dynamic

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/leenkabha/llm_cache/internal/cachequeue"
)

// Queue is a swappable cachequeue.Queue.
//
// Enqueue always goes to the current queue. Consuming is per queue: Run starts a
// worker for the current queue, and Swap starts one for the new queue. The old
// worker is NOT stopped by Swap -- it is told to stop leasing when the manager
// calls StopWorker, which lets in-flight work finish first (see below), so jobs
// already sitting in the old queue keep being processed while it drains.
//
// A worker's handler runs on a context detached from the worker's own, so
// stopping a worker never cancels a cache update that is half done: the worker
// stops leasing, the running handler completes, its job is acknowledged, and only
// then does the worker's Run return.
type Queue struct {
	mu      sync.Mutex
	cur     cachequeue.Queue
	handler func(context.Context, cachequeue.Job) error
	runCtx  context.Context
	workers map[cachequeue.Queue]*worker
	wg      sync.WaitGroup
}

type worker struct {
	cancel context.CancelFunc
	done   chan struct{}
}

var (
	_ cachequeue.Queue   = (*Queue)(nil)
	_ cachequeue.Depther = (*Queue)(nil)
)

func NewQueue(initial cachequeue.Queue) *Queue {
	return &Queue{cur: initial, workers: map[cachequeue.Queue]*worker{}}
}

func (d *Queue) Current() cachequeue.Queue {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.cur
}

func (d *Queue) Enqueue(ctx context.Context, job cachequeue.Job) error {
	return d.Current().Enqueue(ctx, job)
}

// Run implements cachequeue.Queue. It starts the worker for the current queue
// and returns when ctx is canceled and every worker has finished.
func (d *Queue) Run(ctx context.Context, handler func(context.Context, cachequeue.Job) error) {
	d.mu.Lock()
	d.handler, d.runCtx = handler, ctx
	d.startLocked(d.cur)
	d.mu.Unlock()
	<-ctx.Done()
	d.wg.Wait()
}

func (d *Queue) startLocked(q cachequeue.Queue) {
	if d.handler == nil {
		return // Run has not started yet; it will start the worker
	}
	if w, ok := d.workers[q]; ok {
		select {
		case <-w.done:
		default:
			return // already running
		}
	}
	wctx, cancel := context.WithCancel(d.runCtx)
	w := &worker{cancel: cancel, done: make(chan struct{})}
	d.workers[q] = w
	handler := d.handler
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		defer close(w.done)
		q.Run(wctx, func(_ context.Context, job cachequeue.Job) error {
			return handler(context.WithoutCancel(wctx), job)
		})
	}()
}

// Swap makes next the enqueue target, starts consuming from it, and returns the
// previous queue. The previous queue's worker keeps running until StopWorker.
func (d *Queue) Swap(next cachequeue.Queue) cachequeue.Queue {
	d.mu.Lock()
	defer d.mu.Unlock()
	prev := d.cur
	d.cur = next
	d.startLocked(next)
	return prev
}

// StopWorker stops leasing from q and waits (up to wait) for its in-flight job to
// be settled. It reports whether the worker stopped in time.
func (d *Queue) StopWorker(q cachequeue.Queue, wait time.Duration) bool {
	d.mu.Lock()
	w, ok := d.workers[q]
	d.mu.Unlock()
	if !ok {
		return true
	}
	w.cancel()
	select {
	case <-w.done:
		return true
	case <-time.After(wait):
		return false
	}
}

// Drain lets the worker of q keep consuming until the queue reports empty (when
// it implements cachequeue.Depther) or the window elapses, then stops it. It
// returns how many jobs remain, or -1 if the queue cannot say.
func (d *Queue) Drain(ctx context.Context, q cachequeue.Queue, window time.Duration) int {
	dp, canCount := q.(cachequeue.Depther)
	deadline := time.Now().Add(window)
	remaining := -1
	for {
		if canCount {
			cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			n, err := dp.Depth(cctx)
			cancel()
			if err == nil {
				remaining = n
				if n == 0 {
					break
				}
			}
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			break
		}
		select {
		case <-time.After(200 * time.Millisecond):
		case <-ctx.Done():
		}
	}
	if !d.StopWorker(q, 30*time.Second) {
		log.Printf("queue drain: worker did not stop within 30s")
	}
	return remaining
}

// Depth reports the current queue's backlog when supported.
func (d *Queue) Depth(ctx context.Context) (int, error) {
	if dp, ok := d.Current().(cachequeue.Depther); ok {
		return dp.Depth(ctx)
	}
	return 0, ErrUnsupported
}
