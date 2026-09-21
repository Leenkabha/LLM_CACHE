package dynamic

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/leenkabha/llm_cache/internal/cachequeue"
	"github.com/leenkabha/llm_cache/internal/llm"
	"github.com/leenkabha/llm_cache/internal/persistence"
)

type fixedLLM struct {
	reply   string
	block   chan struct{} // if non-nil, Complete waits for it
	started chan struct{}
}

func (f *fixedLLM) Complete(ctx context.Context, _ string) (string, error) {
	if f.started != nil {
		close(f.started)
	}
	if f.block != nil {
		<-f.block
	}
	return f.reply, nil
}

func TestSwapIsAtomicUnderConcurrentCalls(t *testing.T) {
	d := NewLLM(&fixedLLM{reply: "A"})
	var swapped atomic.Bool
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				wasSwapped := swapped.Load() // loaded BEFORE the call
				got, err := d.Complete(context.Background(), "p")
				if err != nil || (got != "A" && got != "B") {
					t.Errorf("torn result %q, %v", got, err)
					return
				}
				if wasSwapped && got != "B" {
					t.Errorf("a call that began after the swap completed used the old implementation")
					return
				}
			}
		}()
	}
	time.Sleep(20 * time.Millisecond)
	d.Swap(&fixedLLM{reply: "B"})
	swapped.Store(true)
	time.Sleep(30 * time.Millisecond)
	close(stop)
	wg.Wait()
}

func TestInFlightCallFinishesOnOldImplementation(t *testing.T) {
	release := make(chan struct{})
	old := &fixedLLM{reply: "old", block: release, started: make(chan struct{})}
	d := NewLLM(old)
	got := make(chan string, 1)
	go func() {
		r, _ := d.Complete(context.Background(), "p")
		got <- r
	}()
	<-old.started
	prev := d.Swap(&fixedLLM{reply: "new"})
	if prev != llm.Backend(old) {
		t.Fatal("Swap did not return the previous implementation")
	}
	if r, _ := d.Complete(context.Background(), "p"); r != "new" {
		t.Fatalf("call after the swap = %q, want new", r)
	}
	close(release)
	if r := <-got; r != "old" {
		t.Fatalf("in-flight call = %q, want old", r)
	}
}

func TestStoreSwapDelegatesEverything(t *testing.T) {
	a, b := persistence.NewMemoryStore(), persistence.NewMemoryStore()
	_ = a.Save(persistence.Entry{ID: "x", Reply: "in-a"})
	d := NewStore(a)
	if e, ok := d.Load("x"); !ok || e.Reply != "in-a" {
		t.Fatal("load through the wrapper failed")
	}
	if _, err := d.LoadDetailed("nope"); err != persistence.ErrNotFound {
		t.Fatalf("LoadDetailed missing = %v", err)
	}
	d.Swap(b)
	if _, ok := d.Load("x"); ok {
		t.Fatal("wrapper still reads the old store after Swap")
	}
	_ = d.Save(persistence.Entry{ID: "y"})
	if n, _ := b.Size(); n != 1 {
		t.Fatalf("new store size = %d, want 1", n)
	}
	if n, _ := a.Size(); n != 1 {
		t.Fatalf("old store must be untouched, size = %d", n)
	}
}

// memQueue is a minimal in-memory cachequeue.Queue with Depth.
type memQueue struct {
	mu   sync.Mutex
	jobs []cachequeue.Job
}

func (q *memQueue) Enqueue(_ context.Context, j cachequeue.Job) error {
	q.mu.Lock()
	q.jobs = append(q.jobs, j)
	q.mu.Unlock()
	return nil
}
func (q *memQueue) Depth(context.Context) (int, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.jobs), nil
}
func (q *memQueue) Run(ctx context.Context, h func(context.Context, cachequeue.Job) error) {
	for ctx.Err() == nil {
		q.mu.Lock()
		var j *cachequeue.Job
		if len(q.jobs) > 0 {
			j = &q.jobs[0]
		}
		q.mu.Unlock()
		if j == nil {
			time.Sleep(2 * time.Millisecond)
			continue
		}
		if err := h(ctx, *j); err != nil {
			time.Sleep(2 * time.Millisecond)
			continue
		}
		q.mu.Lock() // "ack": remove only after success
		q.jobs = q.jobs[1:]
		q.mu.Unlock()
	}
}

func TestQueueSwapDrainsOldAndLosesNothing(t *testing.T) {
	oldQ, newQ := &memQueue{}, &memQueue{}
	d := NewQueue(oldQ)
	var mu sync.Mutex
	seen := map[string]int{}
	handler := func(_ context.Context, j cachequeue.Job) error {
		mu.Lock()
		seen[j.Prompt]++
		mu.Unlock()
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.Run(ctx, handler); close(done) }()
	time.Sleep(20 * time.Millisecond)

	for i := 0; i < 20; i++ {
		_ = d.Enqueue(ctx, cachequeue.Job{Prompt: "old-" + string(rune('a'+i))})
	}
	prev := d.Swap(newQ)
	for i := 0; i < 20; i++ {
		_ = d.Enqueue(ctx, cachequeue.Job{Prompt: "new-" + string(rune('a'+i))})
	}
	if left := d.Drain(ctx, prev, 5*time.Second); left != 0 {
		t.Fatalf("old queue not drained: %d left", left)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := len(seen)
		mu.Unlock()
		if n == 40 || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 40 {
		t.Fatalf("processed %d of 40 jobs; jobs were lost in the switch", len(seen))
	}
	for p, c := range seen {
		if c != 1 {
			t.Fatalf("job %s handled %d times", p, c)
		}
	}
}

func TestStoppingAWorkerDoesNotCancelTheRunningHandler(t *testing.T) {
	q := &memQueue{}
	d := NewQueue(q)
	started, release := make(chan struct{}), make(chan struct{})
	var finished atomic.Bool
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.Run(ctx, func(hctx context.Context, _ cachequeue.Job) error {
		close(started)
		<-release
		if hctx.Err() != nil {
			return hctx.Err() // would be redelivered
		}
		finished.Store(true)
		return nil
	})
	time.Sleep(10 * time.Millisecond)
	_ = d.Enqueue(ctx, cachequeue.Job{Prompt: "p"})
	<-started
	stopped := make(chan bool, 1)
	go func() { stopped <- d.StopWorker(q, 5*time.Second) }()
	select {
	case <-stopped:
		t.Fatal("StopWorker returned while the handler was still running")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if !<-stopped || !finished.Load() {
		t.Fatal("handler did not complete cleanly after the worker was stopped")
	}
	if n, _ := q.Depth(ctx); n != 0 {
		t.Fatalf("job not acknowledged after successful handling: depth %d", n)
	}
}
