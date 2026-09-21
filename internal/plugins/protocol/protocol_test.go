package protocol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/leenkabha/llm_cache/internal/cachequeue"
	"github.com/leenkabha/llm_cache/internal/persistence"
	"github.com/leenkabha/llm_cache/internal/plugins/safehttp"
	"github.com/leenkabha/llm_cache/internal/vectorstore"
)

func client(t *testing.T, h http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := NewClient(srv.URL, safehttp.NewClient(safehttp.Policy{AllowInsecure: true}, 5*time.Second), "")
	if err != nil {
		t.Fatal(err)
	}
	return c, srv
}

func jsonReply(w http.ResponseWriter, code int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write([]byte(body))
}

func TestLLMFailureModes(t *testing.T) {
	cases := map[string]struct {
		h    http.HandlerFunc
		want string
	}{
		"server error hides the body": {func(w http.ResponseWriter, _ *http.Request) {
			jsonReply(w, 500, `{"error":"internal secret path /etc/shadow api_key=abc123456789"}`)
		}, "HTTP 500"},
		"malformed json": {func(w http.ResponseWriter, _ *http.Request) { jsonReply(w, 200, `{"reply":`) }, "malformed"},
		"empty reply":    {func(w http.ResponseWriter, _ *http.Request) { jsonReply(w, 200, `{"reply":"  "}`) }, "empty reply"},
		"wrong shape":    {func(w http.ResponseWriter, _ *http.Request) { jsonReply(w, 200, `[]`) }, "malformed"},
		"oversized": {func(w http.ResponseWriter, _ *http.Request) {
			jsonReply(w, 200, `{"reply":"`+strings.Repeat("x", 2<<20)+`"}`)
		}, "1 MiB"},
		"redirect refused": {func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "http://169.254.169.254/", http.StatusFound)
		}, "HTTP 302"},
		"trailing data": {func(w http.ResponseWriter, _ *http.Request) { jsonReply(w, 200, `{"reply":"ok"} {"reply":"again"}`) }, "malformed"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			cl, _ := client(t, c.h)
			_, err := NewLLM(cl, "m").Complete(context.Background(), "hi")
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want %q", err, c.want)
			}
			for _, leak := range []string{"/etc/shadow", "abc123456789", "169.254"} {
				if strings.Contains(err.Error(), leak) {
					t.Fatalf("plugin-controlled text leaked into the error: %v", err)
				}
			}
		})
	}
}

func TestLLMTimeoutAndCancellation(t *testing.T) {
	cl, _ := client(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(3 * time.Second):
		}
	})
	cl.Timeout = 100 * time.Millisecond
	start := time.Now()
	if _, err := NewLLM(cl, "m").Complete(context.Background(), "hi"); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err = %v, want a timeout", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("the timeout was not enforced")
	}
	cl.Timeout = 5 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := NewLLM(cl, "m").Complete(ctx, "hi"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
}

func TestLLMSendsBearerAndModel(t *testing.T) {
	var gotAuth, gotBody string
	cl, _ := client(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		var b map[string]string
		_ = json.NewDecoder(r.Body).Decode(&b)
		gotBody = b["model"] + "/" + b["prompt"]
		jsonReply(w, 200, `{"reply":"ok"}`)
	})
	cl.Token = "tok"
	if _, err := NewLLM(cl, "gpt-x").Complete(context.Background(), "hello"); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer tok" || gotBody != "gpt-x/hello" {
		t.Fatalf("auth=%q body=%q", gotAuth, gotBody)
	}
}

func TestEmbedderRejectsBadVectors(t *testing.T) {
	cases := map[string]string{
		"dim mismatch": `{"vector":[1,0,0],"dim":2,"model":"m"}`,
		"empty":        `{"vector":[],"dim":0,"model":"m"}`,
		"missing":      `{"dim":3}`,
	}
	for name, body := range cases {
		cl, _ := client(t, func(w http.ResponseWriter, _ *http.Request) { jsonReply(w, 200, body) })
		if _, err := NewEmbedder(cl).Embed(context.Background(), "x"); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	// Non-finite values cannot be encoded in JSON, so the validator is exercised directly.
	if err := ValidateVector([]float64{1, nan()}, 2); err == nil {
		t.Error("NaN accepted")
	}
	if err := ValidateVector(make([]float64, MaxEmbeddingDim+1), MaxEmbeddingDim+1); err == nil {
		t.Error("oversized vector accepted")
	}
}

func nan() float64 { var z float64; return z / z }

func TestVectorStoreRejectsViolationsOfTheSearchContract(t *testing.T) {
	reply := func(body string) *VectorStore {
		cl, _ := client(t, func(w http.ResponseWriter, _ *http.Request) { jsonReply(w, 200, body) })
		return NewVectorStore(cl)
	}
	ctx := context.Background()
	q := []float64{1, 0}
	for name, c := range map[string]struct {
		body string
		k    int
	}{
		"too many matches": {`{"matches":[{"id":"a","distance":0.1},{"id":"b","distance":0.2}]}`, 1},
		"unsorted":         {`{"matches":[{"id":"a","distance":0.3},{"id":"b","distance":0.1}]}`, 2},
		"above threshold":  {`{"matches":[{"id":"a","distance":0.9}]}`, 1},
		"unsafe id":        {`{"matches":[{"id":"../x","distance":0.1}]}`, 1},
		"blank id":         {`{"matches":[{"id":"","distance":0.1}]}`, 1},
		"missing array":    {`{}`, 1},
	} {
		if _, err := reply(c.body).Search(ctx, q, c.k, 0.5); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	ms, err := reply(`{"matches":[{"id":"a","distance":0.1},{"id":"b","distance":0.5}]}`).Search(ctx, q, 2, 0.5)
	if err != nil || len(ms) != 2 {
		t.Fatalf("valid reply rejected: %v", err)
	}
	if _, err := reply(`{"matches":[]}`).Search(ctx, q, 0, 0.5); err == nil {
		t.Error("top_k=0 accepted")
	}
	if _, err := reply(`{"id":"has/slash"}`).Upsert(ctx, q); err == nil {
		t.Error("unsafe upsert id accepted")
	}
	if _, err := reply(`{"restored":1}`).Rebuild(ctx, []vectorstore.RebuildEntry{{ID: "a", Vector: q}, {ID: "b", Vector: q}}); err == nil {
		t.Error("partial rebuild accepted")
	}
	if _, err := reply(`{"restored":1}`).Rebuild(ctx, []vectorstore.RebuildEntry{{ID: "a/b", Vector: q}}); err == nil {
		t.Error("rebuild with an unsafe id accepted")
	}
	if err := reply(`{}`).Delete(ctx, "a/../b"); err == nil {
		t.Error("delete with an unsafe id accepted")
	}
}

func TestPersistenceDistinguishesFailureClasses(t *testing.T) {
	status := atomic.Int32{}
	body := atomic.Value{}
	cl, _ := client(t, func(w http.ResponseWriter, r *http.Request) {
		jsonReply(w, int(status.Load()), body.Load().(string))
	})
	p := NewPersistence(cl)
	check := func(code int, b string, want error) {
		t.Helper()
		status.Store(int32(code))
		body.Store(b)
		_, err := p.LoadDetailed("abc")
		if want == nil {
			if err != nil {
				t.Fatalf("HTTP %d: %v", code, err)
			}
			return
		}
		if !errors.Is(err, want) {
			t.Fatalf("HTTP %d %s: err = %v, want %v", code, b, err, want)
		}
	}
	check(200, `{"id":"abc","prompt":"p","reply":"r","vector":[1],"created_at":"2026-01-01T00:00:00Z"}`, nil)
	check(404, `{"error":{"code":"not_found"}}`, persistence.ErrNotFound)
	check(422, `{"error":{"code":"invalid_data"}}`, persistence.ErrInvalidData)
	check(200, `{"id":"different","prompt":"p","reply":"r"}`, persistence.ErrInvalidData) // id mismatch
	check(200, `not json`, persistence.ErrInvalidData)
	check(500, `{"error":"db down"}`, persistence.ErrBackend)
	check(503, `{}`, persistence.ErrBackend)
	if _, ok := p.Load("abc"); ok {
		t.Fatal("Load must report a failed lookup as absent")
	}

	// A dead endpoint is a backend failure, not "not found".
	dead, srv := client(t, func(http.ResponseWriter, *http.Request) {})
	srv.Close()
	if _, err := NewPersistence(dead).LoadDetailed("abc"); !errors.Is(err, persistence.ErrBackend) {
		t.Fatalf("dead endpoint: %v", err)
	}
	if err := NewPersistence(dead).Save(persistence.Entry{ID: "abc"}); !errors.Is(err, persistence.ErrBackend) {
		t.Fatalf("save on a dead endpoint: %v", err)
	}
	if err := p.Save(persistence.Entry{ID: "bad/id"}); !errors.Is(err, persistence.ErrInvalidData) {
		t.Fatalf("unsafe id: %v", err)
	}
}

func TestPersistenceListPaginationGuards(t *testing.T) {
	cl, _ := client(t, func(w http.ResponseWriter, r *http.Request) {
		jsonReply(w, 200, `{"entries":[{"id":"a","prompt":"p","reply":"r","vector":[1],"created_at":"2026-01-01T00:00:00Z"}],"next_cursor":"same"}`)
	})
	if _, err := NewPersistence(cl).List(); err == nil || !strings.Contains(err.Error(), "advance") && !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("a stuck cursor must fail, got %v", err)
	}
}

// fakeQueue is a lease server whose behaviour tests can script.
type fakeQueue struct {
	mu      sync.Mutex
	jobs    []string
	leased  map[string]string
	acked   []string
	nacked  []string
	ackFail atomic.Bool
}

func newFakeQueue(jobs ...string) *fakeQueue {
	return &fakeQueue{jobs: jobs, leased: map[string]string{}}
}

func (q *fakeQueue) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q.mu.Lock()
		defer q.mu.Unlock()
		switch {
		case r.URL.Path == "/v1/jobs/lease":
			if len(q.jobs) == 0 {
				w.WriteHeader(204)
				return
			}
			id := q.jobs[0]
			q.jobs = q.jobs[1:]
			tok := "tok-" + id
			q.leased[id] = tok
			jsonReply(w, 200, fmt.Sprintf(`{"jobs":[{"id":%q,"prompt":"p-%s","reply":"r","vector":[1,0],"lease_token":%q,"attempt":1}]}`, id, id, tok))
		case strings.HasSuffix(r.URL.Path, "/ack"):
			if q.ackFail.Load() {
				jsonReply(w, 500, `{}`)
				return
			}
			id := strings.Split(r.URL.Path, "/")[3]
			q.acked = append(q.acked, id)
			delete(q.leased, id)
			jsonReply(w, 200, `{}`)
		case strings.HasSuffix(r.URL.Path, "/nack"):
			id := strings.Split(r.URL.Path, "/")[3]
			q.nacked = append(q.nacked, id)
			delete(q.leased, id)
			q.jobs = append(q.jobs, id) // visible again
			jsonReply(w, 200, `{}`)
		case r.URL.Path == "/v1/jobs":
			jsonReply(w, 201, `{"id":"new-1"}`)
		default:
			http.NotFound(w, r)
		}
	}
}

func TestQueueAcksOnlyAfterHandlerSuccessAndNacksOnFailure(t *testing.T) {
	fq := newFakeQueue("j1")
	cl, _ := client(t, fq.handler())
	q := NewQueue(cl, QueueOptions{IdleBackoff: 5 * time.Millisecond, LeaseWait: time.Second})
	var calls atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		q.Run(ctx, func(_ context.Context, j cachequeue.Job) error {
			fq.mu.Lock()
			ackedDuringHandler := len(fq.acked)
			fq.mu.Unlock()
			if ackedDuringHandler != 0 {
				t.Error("job acknowledged before the handler finished")
			}
			if calls.Add(1) == 1 {
				return errors.New("boom")
			}
			cancel()
			return nil
		})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not finish")
	}
	fq.mu.Lock()
	defer fq.mu.Unlock()
	if len(fq.nacked) != 1 || len(fq.acked) != 1 || calls.Load() != 2 {
		t.Fatalf("nacked=%v acked=%v calls=%d", fq.nacked, fq.acked, calls.Load())
	}
}

func TestQueueHandlerPanicNacksInsteadOfCrashing(t *testing.T) {
	fq := newFakeQueue("j1")
	cl, _ := client(t, fq.handler())
	q := NewQueue(cl, QueueOptions{IdleBackoff: 5 * time.Millisecond, LeaseWait: time.Second})
	ctx, cancel := context.WithCancel(context.Background())
	var n atomic.Int32
	done := make(chan struct{})
	go func() {
		q.Run(ctx, func(context.Context, cachequeue.Job) error {
			if n.Add(1) == 1 {
				panic("handler bug")
			}
			cancel()
			return nil
		})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not recover from the panic")
	}
	if len(fq.nacked) != 1 {
		t.Fatalf("nacked = %v", fq.nacked)
	}
}

func TestQueueAckFailureIsSurvivedNotFatal(t *testing.T) {
	fq := newFakeQueue("j1")
	fq.ackFail.Store(true)
	cl, _ := client(t, fq.handler())
	q := NewQueue(cl, QueueOptions{IdleBackoff: 5 * time.Millisecond, LeaseWait: time.Second})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		q.Run(ctx, func(context.Context, cachequeue.Job) error { cancel(); return nil })
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("worker hung on a failing ack")
	}
}

func TestQueueEnqueueRequiresDurableConfirmation(t *testing.T) {
	for name, c := range map[string]struct {
		code int
		body string
	}{
		"200 not 201": {200, `{"id":"x"}`}, "no id": {201, `{}`}, "unsafe id": {201, `{"id":"a/b"}`}, "server error": {500, `{}`},
	} {
		cl, _ := client(t, func(w http.ResponseWriter, _ *http.Request) { jsonReply(w, c.code, c.body) })
		if err := NewQueue(cl, QueueOptions{}).Enqueue(context.Background(), cachequeue.Job{Prompt: "p", Reply: "r", Vector: []float64{1}}); err == nil {
			t.Errorf("%s: enqueue reported success", name)
		}
	}
	cl, _ := client(t, func(w http.ResponseWriter, _ *http.Request) { jsonReply(w, 201, `{"id":"job-1"}`) })
	if err := NewQueue(cl, QueueOptions{}).Enqueue(context.Background(), cachequeue.Job{Prompt: "p", Reply: "r", Vector: []float64{1}}); err != nil {
		t.Fatal(err)
	}
}

func TestPolicyBatchesInOrderAndVictimSeesPriorEvents(t *testing.T) {
	var mu sync.Mutex
	var applied []string
	var batches int
	cl, _ := client(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/v1/policy":
			jsonReply(w, 200, `{"name":"remote"}`)
		case "/v1/events":
			var b struct {
				Events []struct{ Op, ID string } `json:"events"`
			}
			_ = json.NewDecoder(r.Body).Decode(&b)
			batches++
			for _, e := range b.Events {
				applied = append(applied, e.Op+":"+e.ID)
			}
			jsonReply(w, 200, `{}`)
		case "/v1/victim":
			jsonReply(w, 200, fmt.Sprintf(`{"id":"v","ok":true,"seen":%d}`, len(applied)))
		default:
			jsonReply(w, 200, `{}`)
		}
	})
	p, err := NewPolicy(context.Background(), cl)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if p.Name() != "remote" {
		t.Fatalf("name = %q", p.Name())
	}
	for i := 0; i < 50; i++ {
		p.OnInsert(fmt.Sprintf("id%d", i))
		p.OnHit(fmt.Sprintf("id%d", i))
	}
	if id, ok := p.Victim(); !ok || id != "v" {
		t.Fatalf("victim = %q %v", id, ok)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(applied) != 100 {
		t.Fatalf("Victim returned before all %d earlier events were delivered (%d applied)", 100, len(applied))
	}
	for i := 0; i < 50; i++ {
		if applied[2*i] != fmt.Sprintf("insert:id%d", i) || applied[2*i+1] != fmt.Sprintf("hit:id%d", i) {
			t.Fatalf("events out of order at %d: %v", i, applied[2*i:2*i+2])
		}
	}
	if batches >= 100 {
		t.Fatalf("no batching happened: %d requests for 100 events", batches)
	}
}

func TestPolicyRejectsBadNameAndUnsafeVictim(t *testing.T) {
	cl, _ := client(t, func(w http.ResponseWriter, _ *http.Request) { jsonReply(w, 200, `{"name":""}`) })
	if _, err := NewPolicy(context.Background(), cl); err == nil {
		t.Fatal("a policy without a name was accepted")
	}
	cl, _ = client(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/policy" {
			jsonReply(w, 200, `{"name":"p"}`)
			return
		}
		jsonReply(w, 200, `{"id":"../etc","ok":true}`)
	})
	p, err := NewPolicy(context.Background(), cl)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if _, ok := p.Victim(); ok {
		t.Fatal("an unsafe victim id was returned to the orchestrator")
	}
}

func TestPolicyReportsDroppedOrFailedEventsInHealth(t *testing.T) {
	cl, _ := client(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/policy":
			jsonReply(w, 200, `{"name":"p"}`)
		case "/v1/events":
			jsonReply(w, 500, `{}`)
		default:
			jsonReply(w, 200, `{}`)
		}
	})
	p, _ := NewPolicy(context.Background(), cl)
	defer p.Close()
	p.OnInsert("a")
	p.Victim() // forces delivery
	if err := p.Health(context.Background()); err == nil {
		t.Fatal("health did not report events the plugin failed to receive")
	}
}

func TestValidID(t *testing.T) {
	for _, ok := range []string{"a", "abc-123", "A.b_c~d", strings.Repeat("x", 128)} {
		if !ValidID(ok) {
			t.Errorf("%q rejected", ok)
		}
	}
	for _, bad := range []string{"", ".", "..", "a/b", "a b", "a?b", "a#b", "ä", "a%2fb", strings.Repeat("x", 129)} {
		if ValidID(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
}
