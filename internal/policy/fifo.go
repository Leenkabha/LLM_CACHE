package policy

import (
	"container/list"
	"sync"

	"github.com/leenkabha/llm_cache/internal/config"
)

// PolicyFIFO selects the example first-in, first-out eviction policy.
const PolicyFIFO = "fifo"

// This file is a complete adapter template. Copy it into this package, rename
// the type/registration name, and replace the selection algorithm.
func init() {
	Register(PolicyFIFO, func(config.Config) (EvictionPolicy, error) {
		return &fifoPolicy{order: list.New(), items: make(map[string]*list.Element)}, nil
	})
}

type fifoPolicy struct {
	mu    sync.Mutex
	order *list.List
	items map[string]*list.Element
}

var _ EvictionPolicy = (*fifoPolicy)(nil)

func (*fifoPolicy) Name() string { return PolicyFIFO }

// Accesses do not change insertion order in FIFO.
func (*fifoPolicy) OnHit(string) {}

func (p *fifoPolicy) OnInsert(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, exists := p.items[id]; exists {
		return
	}
	p.items[id] = p.order.PushBack(id)
}

func (p *fifoPolicy) OnDelete(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if item, exists := p.items[id]; exists {
		p.order.Remove(item)
		delete(p.items, id)
	}
}

// Selecting a victim does not remove it. The orchestrator calls OnDelete
// only after deleting the stored reply and vector successfully.
func (p *fifoPolicy) Victim() (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if first := p.order.Front(); first != nil {
		return first.Value.(string), true
	}
	return "", false
}

func (p *fifoPolicy) Flush() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.order.Init()
	p.items = make(map[string]*list.Element)
}
