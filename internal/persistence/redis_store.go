package persistence

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	defaultRedisTimeout = 5 * time.Second
	defaultRedisPrefix  = "llm_cache"
)

// RedisStore persists policy-agnostic cache entries in Redis.
//
// The cache entry schema intentionally stays independent from eviction policy
// metadata. Policy implementations may use their own Redis keys later, while
// this store remains responsible only for durable cache data.
type RedisStore struct {
	client *redis.Client
	prefix string
}

// NewRedisStore connects to Redis and returns a Store implementation.
func NewRedisStore(addr string) (*RedisStore, error) {
	store := &RedisStore{
		client: redis.NewClient(&redis.Options{Addr: addr}),
		prefix: defaultRedisPrefix,
	}

	ctx, cancel := context.WithTimeout(context.Background(), defaultRedisTimeout)
	defer cancel()
	if err := store.client.Ping(ctx).Err(); err != nil {
		_ = store.client.Close()
		return nil, fmt.Errorf("redis ping failed: %w", err)
	}
	return store, nil
}

func (r *RedisStore) Save(entry Entry) error {
	ctx, cancel := context.WithTimeout(context.Background(), defaultRedisTimeout)
	defer cancel()

	entry.Vector = cloneVector(entry.Vector)
	data, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("marshal cache entry: %w", err)
	}

	pipe := r.client.TxPipeline()
	pipe.Set(ctx, r.entryKey(entry.ID), data, 0)
	pipe.SAdd(ctx, r.idsKey(), entry.ID)
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("save cache entry %q: %w", entry.ID, err)
	}
	return nil
}

func (r *RedisStore) Load(id string) (Entry, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), defaultRedisTimeout)
	defer cancel()

	data, err := r.client.Get(ctx, r.entryKey(id)).Bytes()
	if errors.Is(err, redis.Nil) {
		return Entry{}, false
	}
	if err != nil {
		return Entry{}, false
	}

	entry, err := decodeEntry(data)
	if err != nil {
		return Entry{}, false
	}
	return entry, true
}

func (r *RedisStore) List() ([]Entry, error) {
	ctx, cancel := context.WithTimeout(context.Background(), defaultRedisTimeout)
	defer cancel()

	ids, err := r.client.SMembers(ctx, r.idsKey()).Result()
	if err != nil {
		return nil, fmt.Errorf("list cache entry ids: %w", err)
	}

	entries := make([]Entry, 0, len(ids))
	for _, id := range ids {
		data, err := r.client.Get(ctx, r.entryKey(id)).Bytes()
		if errors.Is(err, redis.Nil) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("load cache entry %q: %w", id, err)
		}
		entry, err := decodeEntry(data)
		if err != nil {
			return nil, fmt.Errorf("decode cache entry %q: %w", id, err)
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

func (r *RedisStore) Size() (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), defaultRedisTimeout)
	defer cancel()

	size, err := r.client.SCard(ctx, r.idsKey()).Result()
	if err != nil {
		return 0, fmt.Errorf("cache size: %w", err)
	}
	return int(size), nil
}

func (r *RedisStore) Delete(id string) error {
	ctx, cancel := context.WithTimeout(context.Background(), defaultRedisTimeout)
	defer cancel()

	pipe := r.client.TxPipeline()
	pipe.Del(ctx, r.entryKey(id))
	pipe.SRem(ctx, r.idsKey(), id)
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("delete cache entry %q: %w", id, err)
	}
	return nil
}

func (r *RedisStore) Flush() error {
	ctx, cancel := context.WithTimeout(context.Background(), defaultRedisTimeout)
	defer cancel()

	ids, err := r.client.SMembers(ctx, r.idsKey()).Result()
	if err != nil {
		return fmt.Errorf("list cache entry ids for flush: %w", err)
	}

	keys := make([]string, 0, len(ids)+1)
	for _, id := range ids {
		keys = append(keys, r.entryKey(id))
	}
	keys = append(keys, r.idsKey())

	if err := r.client.Del(ctx, keys...).Err(); err != nil {
		return fmt.Errorf("flush cache entries: %w", err)
	}
	return nil
}

func (r *RedisStore) Health() error {
	ctx, cancel := context.WithTimeout(context.Background(), defaultRedisTimeout)
	defer cancel()
	if err := r.client.Ping(ctx).Err(); err != nil {
		return fmt.Errorf("redis health check failed: %w", err)
	}
	return nil
}

func (r *RedisStore) entryKey(id string) string {
	return fmt.Sprintf("%s:entry:%s", r.prefix, id)
}

func (r *RedisStore) idsKey() string {
	return r.prefix + ":entries"
}

func decodeEntry(data []byte) (Entry, error) {
	var entry Entry
	if err := json.Unmarshal(data, &entry); err != nil {
		return Entry{}, err
	}
	entry.Vector = cloneVector(entry.Vector)
	return entry, nil
}
