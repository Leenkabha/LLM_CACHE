package protocol

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/leenkabha/llm_cache/internal/policy"
)

// Eviction-policy contract (v1):
//
//	GET  /v1/policy  -> {"name":"my-policy"}
//	POST /v1/events  {"events":[{"op":"hit|insert|delete","id":"a"}, ...]} -> 200
//	GET  /v1/victim  -> {"id":"a","ok":true} | {"ok":false}      (selects, never deletes)
//	POST /v1/flush   -> 200                                       (policy stays usable)
//	GET  /health
//
// Events are applied in the order given. Unknown ids in "hit" and "delete" must
// be harmless and a repeated "insert" must not create duplicate state.
//
// Latency: every OnHit/OnInsert/OnDelete would cost a network round trip on the
// query path. The adapter therefore sends events from a background goroutine, in
// order and in batches (POST /v1/events carries an array for exactly this
// reason), and Victim/Flush first wait until every earlier event has been
// delivered so they observe them. The price is that a hit is applied by the
// plugin slightly after the response is sent, and that a plugin outage can drop
// hit notifications (counted and reported through Health).

const (
	eventBuffer   = 4096
	maxBatch      = 128
	insertBlock   = 250 * time.Millisecond // insert/delete wait this long for buffer space
	barrierWait   = 5 * time.Second
	eventPostWait = 3 * time.Second
)

type policyEvent struct {
	Op string `json:"op"`
	ID string `json:"id"`
}

type queued struct {
	ev      policyEvent
	barrier chan struct{} // non-nil: a synchronisation marker, ev unused
}

// Policy is the remote adapter for policy.EvictionPolicy.
type Policy struct {
	c    *Client
	name string

	ch      chan queued
	stop    chan struct{}
	done    chan struct{}
	once    sync.Once
	dropped atomic.Int64
	failed  atomic.Int64
	lastErr atomic.Value // string
}

var _ policy.EvictionPolicy = (*Policy)(nil)

// NewPolicy contacts the plugin for its name and starts the event sender.
func NewPolicy(ctx context.Context, c *Client) (*Policy, error) {
	cc := *c
	if cc.MaxResponse == 0 || cc.MaxResponse > 1<<20 {
		cc.MaxResponse = 1 << 20
	}
	if cc.Timeout == 0 || cc.Timeout > 5*time.Second {
		cc.Timeout = 5 * time.Second
	}
	var out struct {
		Name string `json:"name"`
	}
	if _, err := cc.Do(ctx, http.MethodGet, "/v1/policy", nil, &out); err != nil {
		return nil, fmt.Errorf("policy plugin: %w", err)
	}
	if out.Name == "" || len(out.Name) > 64 {
		return nil, fmt.Errorf("policy plugin: %w (missing name)", ErrMalformed)
	}
	p := &Policy{c: &cc, name: out.Name, ch: make(chan queued, eventBuffer), stop: make(chan struct{}), done: make(chan struct{})}
	go p.sender()
	return p, nil
}

func (p *Policy) Name() string { return p.name }

func (p *Policy) send(op, id string, block bool) {
	if !ValidID(id) {
		return
	}
	q := queued{ev: policyEvent{op, id}}
	select {
	case p.ch <- q:
		return
	default:
	}
	if !block {
		p.dropped.Add(1)
		return
	}
	t := time.NewTimer(insertBlock)
	defer t.Stop()
	select {
	case p.ch <- q:
	case <-t.C:
		p.dropped.Add(1)
	case <-p.stop:
	}
}

func (p *Policy) OnHit(id string)    { p.send("hit", id, false) }
func (p *Policy) OnInsert(id string) { p.send("insert", id, true) }
func (p *Policy) OnDelete(id string) { p.send("delete", id, true) }

// sender drains the queue in order, batching consecutive events.
func (p *Policy) sender() {
	defer close(p.done)
	var batch []policyEvent
	var barriers []chan struct{}
	flush := func() {
		if len(batch) > 0 {
			ctx, cancel := context.WithTimeout(context.Background(), eventPostWait)
			_, err := p.c.Do(ctx, http.MethodPost, "/v1/events", map[string]any{"events": batch}, nil)
			cancel()
			if err != nil {
				p.failed.Add(int64(len(batch)))
				p.lastErr.Store(err.Error())
				log.Printf("policy plugin: delivering %d events failed: %v", len(batch), err)
			}
			batch = batch[:0]
		}
		for _, b := range barriers {
			close(b)
		}
		barriers = barriers[:0]
	}
	for {
		var first queued
		select {
		case first = <-p.ch:
		case <-p.stop:
			// Deliver what is already buffered, then exit.
			for {
				select {
				case q := <-p.ch:
					p.add(&batch, &barriers, q)
				default:
					flush()
					return
				}
			}
		}
		p.add(&batch, &barriers, first)
	drain:
		for len(batch) < maxBatch {
			select {
			case q := <-p.ch:
				p.add(&batch, &barriers, q)
				if q.barrier != nil {
					break drain // deliver up to the barrier promptly
				}
			default:
				break drain
			}
		}
		flush()
	}
}

func (p *Policy) add(batch *[]policyEvent, barriers *[]chan struct{}, q queued) {
	if q.barrier != nil {
		*barriers = append(*barriers, q.barrier)
		return
	}
	*batch = append(*batch, q.ev)
}

// syncEvents blocks until every event queued before the call has been sent.
func (p *Policy) syncEvents() bool {
	b := make(chan struct{})
	select {
	case p.ch <- queued{barrier: b}:
	case <-time.After(barrierWait):
		return false
	case <-p.stop:
		return false
	}
	select {
	case <-b:
		return true
	case <-time.After(barrierWait):
		return false
	}
}

// Victim implements policy.EvictionPolicy. A plugin failure yields (“”, false)
// and is logged: the orchestrator then reports that the policy has no victim.
func (p *Policy) Victim() (string, bool) {
	if !p.syncEvents() {
		log.Printf("policy plugin: could not deliver pending events before Victim")
	}
	ctx, cancel := context.WithTimeout(context.Background(), p.c.Timeout)
	defer cancel()
	var out struct {
		ID string `json:"id"`
		OK bool   `json:"ok"`
	}
	if _, err := p.c.Do(ctx, http.MethodGet, "/v1/victim", nil, &out); err != nil {
		log.Printf("policy plugin: victim failed: %v", err)
		return "", false
	}
	if !out.OK {
		return "", false
	}
	if !ValidID(out.ID) {
		log.Printf("policy plugin: victim returned an unsafe id")
		return "", false
	}
	return out.ID, true
}

func (p *Policy) Flush() {
	p.syncEvents()
	ctx, cancel := context.WithTimeout(context.Background(), p.c.Timeout)
	defer cancel()
	if _, err := p.c.Do(ctx, http.MethodPost, "/v1/flush", struct{}{}, nil); err != nil {
		log.Printf("policy plugin: flush failed: %v", err)
	}
}

// Health checks the plugin and reports lost events since the last check.
func (p *Policy) Health(ctx context.Context) error {
	if err := p.c.Health(ctx); err != nil {
		return err
	}
	if n := p.dropped.Load(); n > 0 {
		return fmt.Errorf("policy plugin dropped %d events (buffer full)", n)
	}
	if n := p.failed.Load(); n > 0 {
		msg, _ := p.lastErr.Load().(string)
		return errors.New(fmt.Sprintf("policy plugin failed to receive %d events: %s", n, msg))
	}
	return nil
}

// Close stops the sender after delivering buffered events.
func (p *Policy) Close() {
	p.once.Do(func() { close(p.stop) })
	<-p.done
}
