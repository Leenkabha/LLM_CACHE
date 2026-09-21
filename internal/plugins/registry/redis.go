package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/leenkabha/llm_cache/internal/plugins"
)

// Namespace is the Redis key prefix for plugin platform data. It differs from
// the semantic cache's "llm_cache:" prefix, so a cache flush can never touch
// plugin records and vice versa.
const Namespace = "llm_cache_plugins"

const redisTimeout = 5 * time.Second

// Redis is the durable Store.
type Redis struct {
	c *redis.Client
}

// NewRedis connects to Redis at addr.
func NewRedis(addr string) (*Redis, error) {
	r := &Redis{c: redis.NewClient(&redis.Options{Addr: addr})}
	if err := r.Ping(); err != nil {
		_ = r.c.Close()
		return nil, err
	}
	return r, nil
}

func ctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), redisTimeout)
}

func recKey(id string) string { return Namespace + ":record:" + id }
func logKey(id string) string { return Namespace + ":logs:" + id }

const (
	idsKey    = Namespace + ":ids"
	activeKey = Namespace + ":active"
	auditKey  = Namespace + ":audit"
)

func (r *Redis) Ping() error {
	c, cancel := ctx()
	defer cancel()
	if err := r.c.Ping(c).Err(); err != nil {
		return fmt.Errorf("plugin registry redis ping failed: %w", err)
	}
	return nil
}

func (r *Redis) Get(id string) (*Record, error) {
	c, cancel := ctx()
	defer cancel()
	b, err := r.c.Get(c, recKey(id)).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return decode(b)
}

func (r *Redis) List() ([]*Record, error) {
	c, cancel := ctx()
	defer cancel()
	ids, err := r.c.SMembers(c, idsKey).Result()
	if err != nil {
		return nil, err
	}
	out := make([]*Record, 0, len(ids))
	for _, id := range ids {
		rec, err := r.Get(id)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	sortRecords(out)
	return out, nil
}

func (r *Redis) Create(rec *Record) error {
	c, cancel := ctx()
	defer cancel()
	cp := rec.Clone()
	cp.Rev = 1
	b, err := json.Marshal(cp)
	if err != nil {
		return err
	}
	ok, err := r.c.SetNX(c, recKey(rec.ID), b, 0).Result()
	if err != nil {
		return err
	}
	if !ok {
		return ErrExists
	}
	if err := r.c.SAdd(c, idsKey, rec.ID).Err(); err != nil {
		return err
	}
	rec.Rev = 1
	return nil
}

// Update is optimistic: WATCH the key, apply fn, write in a transaction, retry
// on contention.
func (r *Redis) Update(id string, fn func(*Record) error) (*Record, error) {
	key := recKey(id)
	var result *Record
	for attempt := 0; attempt < 60; attempt++ {
		c, cancel := ctx()
		err := r.c.Watch(c, func(tx *redis.Tx) error {
			b, err := tx.Get(c, key).Bytes()
			if errors.Is(err, redis.Nil) {
				return ErrNotFound
			}
			if err != nil {
				return err
			}
			rec, err := decode(b)
			if err != nil {
				return err
			}
			if err := fn(rec); err != nil {
				return err
			}
			rec.Rev++
			rec.UpdatedAt = time.Now().UTC()
			nb, err := json.Marshal(rec)
			if err != nil {
				return err
			}
			_, err = tx.TxPipelined(c, func(p redis.Pipeliner) error {
				p.Set(c, key, nb, 0)
				return nil
			})
			if err == nil {
				result, _ = decode(nb)
			}
			return err
		}, key)
		cancel()
		if errors.Is(err, redis.TxFailedErr) {
			// Lost the optimistic race: back off with jitter and retry.
			time.Sleep(time.Duration(1+rand.Intn(5+attempt)) * time.Millisecond)
			continue
		}
		return result, err
	}
	return nil, ErrConflict
}

func (r *Redis) Delete(id string) error {
	c, cancel := ctx()
	defer cancel()
	n, err := r.c.Del(c, recKey(id)).Result()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	pipe := r.c.TxPipeline()
	pipe.SRem(c, idsKey, id)
	pipe.Del(c, logKey(id))
	_, err = pipe.Exec(c)
	return err
}

func (r *Redis) Active() (map[plugins.Type]string, error) {
	c, cancel := ctx()
	defer cancel()
	m, err := r.c.HGetAll(c, activeKey).Result()
	if err != nil {
		return nil, err
	}
	out := make(map[plugins.Type]string, len(m))
	for k, v := range m {
		out[plugins.Type(k)] = v
	}
	return out, nil
}

func (r *Redis) SetActive(slot plugins.Type, id string) error {
	c, cancel := ctx()
	defer cancel()
	return r.c.HSet(c, activeKey, string(slot), id).Err()
}

func (r *Redis) ClearActive(slot plugins.Type) error {
	c, cancel := ctx()
	defer cancel()
	return r.c.HDel(c, activeKey, string(slot)).Err()
}

func (r *Redis) AppendAudit(e Event) error {
	c, cancel := ctx()
	defer cancel()
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	pipe := r.c.TxPipeline()
	pipe.LPush(c, auditKey, b)
	pipe.LTrim(c, auditKey, 0, maxAuditEvents-1)
	_, err = pipe.Exec(c)
	return err
}

func (r *Redis) Audit(pluginID string, limit int) ([]Event, error) {
	c, cancel := ctx()
	defer cancel()
	raw, err := r.c.LRange(c, auditKey, 0, maxAuditEvents-1).Result()
	if err != nil {
		return nil, err
	}
	var out []Event
	for _, s := range raw {
		var e Event
		if json.Unmarshal([]byte(s), &e) != nil {
			continue
		}
		if pluginID == "" || e.PluginID == pluginID {
			out = append(out, e)
			if limit > 0 && len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

func (r *Redis) AppendLog(id, line string) error {
	c, cancel := ctx()
	defer cancel()
	if len(line) > maxLogLineLen {
		line = line[:maxLogLineLen]
	}
	pipe := r.c.TxPipeline()
	pipe.RPush(c, logKey(id), line)
	pipe.LTrim(c, logKey(id), -maxLogLines, -1)
	_, err := pipe.Exec(c)
	return err
}

func (r *Redis) Logs(id string, limit int) ([]string, error) {
	c, cancel := ctx()
	defer cancel()
	start := int64(0)
	if limit > 0 {
		start = int64(-limit)
	}
	return r.c.LRange(c, logKey(id), start, -1).Result()
}
