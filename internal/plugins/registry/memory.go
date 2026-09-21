package registry

import (
	"encoding/json"
	"sort"
	"sync"
	"time"

	"github.com/leenkabha/llm_cache/internal/plugins"
)

// Memory is an in-memory Store for tests and for running the platform without
// Redis. Records are copied through JSON so callers cannot alias stored state.
type Memory struct {
	mu     sync.Mutex
	recs   map[string][]byte
	active map[plugins.Type]string
	audit  []Event
	logs   map[string][]string
}

func NewMemory() *Memory {
	return &Memory{recs: map[string][]byte{}, active: map[plugins.Type]string{}, logs: map[string][]string{}}
}

func decode(b []byte) (*Record, error) {
	var r Record
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

func (m *Memory) Get(id string) (*Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.recs[id]
	if !ok {
		return nil, ErrNotFound
	}
	return decode(b)
}

func (m *Memory) List() ([]*Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Record, 0, len(m.recs))
	for _, b := range m.recs {
		r, err := decode(b)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	sortRecords(out)
	return out, nil
}

func sortRecords(rs []*Record) {
	sort.Slice(rs, func(i, j int) bool {
		if !rs[i].CreatedAt.Equal(rs[j].CreatedAt) {
			return rs[i].CreatedAt.Before(rs[j].CreatedAt)
		}
		return rs[i].ID < rs[j].ID
	})
}

func (m *Memory) Create(r *Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.recs[r.ID]; ok {
		return ErrExists
	}
	c := r.Clone()
	c.Rev = 1
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	m.recs[r.ID] = b
	r.Rev = 1
	return nil
}

func (m *Memory) Update(id string, fn func(*Record) error) (*Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.recs[id]
	if !ok {
		return nil, ErrNotFound
	}
	r, err := decode(b)
	if err != nil {
		return nil, err
	}
	if err := fn(r); err != nil {
		return nil, err
	}
	r.Rev++
	r.UpdatedAt = time.Now().UTC()
	nb, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	m.recs[id] = nb
	return decode(nb)
}

func (m *Memory) Delete(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.recs[id]; !ok {
		return ErrNotFound
	}
	delete(m.recs, id)
	delete(m.logs, id)
	return nil
}

func (m *Memory) Active() (map[plugins.Type]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[plugins.Type]string, len(m.active))
	for k, v := range m.active {
		out[k] = v
	}
	return out, nil
}

func (m *Memory) SetActive(slot plugins.Type, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.active[slot] = id
	return nil
}

func (m *Memory) ClearActive(slot plugins.Type) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.active, slot)
	return nil
}

func (m *Memory) AppendAudit(e Event) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.audit = append(m.audit, e)
	if len(m.audit) > maxAuditEvents {
		m.audit = m.audit[len(m.audit)-maxAuditEvents:]
	}
	return nil
}

func (m *Memory) Audit(pluginID string, limit int) ([]Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Event
	for i := len(m.audit) - 1; i >= 0; i-- { // newest first
		if pluginID == "" || m.audit[i].PluginID == pluginID {
			out = append(out, m.audit[i])
			if limit > 0 && len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

func (m *Memory) AppendLog(id, line string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(line) > maxLogLineLen {
		line = line[:maxLogLineLen]
	}
	l := append(m.logs[id], line)
	if len(l) > maxLogLines {
		l = l[len(l)-maxLogLines:]
	}
	m.logs[id] = l
	return nil
}

func (m *Memory) Logs(id string, limit int) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.logs[id]
	if limit > 0 && len(l) > limit {
		l = l[len(l)-limit:]
	}
	return append([]string(nil), l...), nil
}

func (m *Memory) Ping() error { return nil }
