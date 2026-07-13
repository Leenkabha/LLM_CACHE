package policy

import (
	"container/list"
	"sync"
)

type lruPolicy struct {
	mu    sync.Mutex
	order *list.List
	items map[string]*list.Element
}

func newLRUPolicy() *lruPolicy {
	return &lruPolicy{
		order: list.New(),
		items: make(map[string]*list.Element),
	}
}

func (p *lruPolicy) Name() string {
	return "lru"
}

func (p *lruPolicy) OnHit(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if element, ok := p.items[id]; ok {
		p.order.MoveToFront(element)
	}
}

func (p *lruPolicy) OnInsert(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if element, ok := p.items[id]; ok {
		p.order.MoveToFront(element)
		return
	}
	p.items[id] = p.order.PushFront(id)
}

func (p *lruPolicy) OnDelete(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if element, ok := p.items[id]; ok {
		p.order.Remove(element)
		delete(p.items, id)
	}
}

func (p *lruPolicy) Victim() (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	element := p.order.Back()
	if element == nil {
		return "", false
	}
	id, ok := element.Value.(string)
	return id, ok
}

func (p *lruPolicy) Flush() {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.order.Init()
	p.items = make(map[string]*list.Element)
}
