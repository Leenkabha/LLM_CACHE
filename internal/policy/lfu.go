package policy

import "sync"

type lfuEntry struct {
	count int
	order int64
}

type lfuPolicy struct {
	mu      sync.Mutex
	entries map[string]lfuEntry
	next    int64
}

func newLFUPolicy() *lfuPolicy {
	return &lfuPolicy{entries: make(map[string]lfuEntry)}
}

func (p *lfuPolicy) Name() string {
	return "lfu"
}

func (p *lfuPolicy) OnHit(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	entry, ok := p.entries[id]
	if !ok {
		return
	}
	entry.count++
	p.entries[id] = entry
}

func (p *lfuPolicy) OnInsert(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if _, ok := p.entries[id]; ok {
		return
	}
	p.entries[id] = lfuEntry{count: 1, order: p.next}
	p.next++
}

func (p *lfuPolicy) OnDelete(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	delete(p.entries, id)
}

func (p *lfuPolicy) Victim() (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	var victimID string
	var victim lfuEntry
	found := false
	for id, entry := range p.entries {
		if !found || entry.count < victim.count || entry.count == victim.count && entry.order < victim.order {
			victimID = id
			victim = entry
			found = true
		}
	}
	return victimID, found
}

func (p *lfuPolicy) Flush() {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.entries = make(map[string]lfuEntry)
	p.next = 0
}
