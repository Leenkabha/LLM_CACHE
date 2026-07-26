// Package cachequeue owns the async cache-update queue.
package cachequeue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/leenkabha/llm_cache/internal/config"
	"github.com/leenkabha/llm_cache/internal/plugin"
)

const (
	defaultRedisTimeout = 5 * time.Second
	defaultStreamName   = "llm_cache:cache_updates"
	defaultGroupName    = "cache-workers"
)

// Job is the durable async cache-update payload.
type Job struct {
	Prompt string
	Reply  string
	Vector []float64
}

// Queue is the async cache-update queue the orchestrator depends on.
//
// This is the pluggable seam: the orchestrator holds a Queue, so the durable
// transport behind it (Redis Streams today) is interchangeable.
type Queue interface {
	// Enqueue appends a cache-update job for asynchronous processing.
	Enqueue(ctx context.Context, job Job) error
	// Run consumes jobs until ctx is canceled, invoking handler for each.
	Run(ctx context.Context, handler func(context.Context, Job) error)
}

// Backend names the built-in queue adapters selectable via configuration.
const (
	// BackendRedis uses Redis Streams as the durable transport (default).
	BackendRedis = "redis"
)

// registry holds every queue adapter, keyed by the name used in QUEUE_BACKEND.
var registry = plugin.NewRegistry[Queue]("cache-update queue backend")

// Register makes a queue adapter available under name.
//
// Call it from an init() in your adapter file. Because adapters live in package
// cachequeue, adding the file is enough -- no existing file changes. Then set
// QUEUE_BACKEND=<name> to select it.
func Register(name string, factory plugin.Factory[Queue]) {
	registry.Register(name, factory)
}

// New builds the Queue selected by cfg.QueueBackend from the registry.
func New(cfg config.Config) (Queue, error) {
	name := cfg.QueueBackend
	if name == "" {
		name = BackendRedis
	}
	return registry.Build(name, cfg)
}

// The built-in Redis Streams adapter registers itself.
func init() {
	Register(BackendRedis, func(cfg config.Config) (Queue, error) {
		return NewRedisStreamQueue(cfg.RedisAddr)
	})
}

// RedisStreamQueue stores cache-update jobs in a Redis Stream.
type RedisStreamQueue struct {
	client   *redis.Client
	stream   string
	group    string
	consumer string
}

// NewRedisStreamQueue connects to Redis and ensures the consumer group exists.
func NewRedisStreamQueue(addr string) (*RedisStreamQueue, error) {
	queue := &RedisStreamQueue{
		client:   redis.NewClient(&redis.Options{Addr: addr}),
		stream:   defaultStreamName,
		group:    defaultGroupName,
		consumer: consumerName(),
	}

	ctx, cancel := context.WithTimeout(context.Background(), defaultRedisTimeout)
	defer cancel()
	if err := queue.client.Ping(ctx).Err(); err != nil {
		_ = queue.client.Close()
		return nil, fmt.Errorf("redis ping failed: %w", err)
	}
	if err := queue.ensureGroup(ctx); err != nil {
		_ = queue.client.Close()
		return nil, err
	}
	return queue, nil
}

func (q *RedisStreamQueue) ensureGroup(ctx context.Context) error {
	err := q.client.XGroupCreateMkStream(ctx, q.stream, q.group, "0").Err()
	if err == nil || strings.Contains(err.Error(), "BUSYGROUP") {
		return nil
	}
	return fmt.Errorf("create redis stream group: %w", err)
}

// Enqueue appends a cache-update job to the Redis Stream.
func (q *RedisStreamQueue) Enqueue(ctx context.Context, job Job) error {
	vectorJSON, err := json.Marshal(job.Vector)
	if err != nil {
		return fmt.Errorf("marshal cache-update vector: %w", err)
	}

	if err := q.client.XAdd(ctx, &redis.XAddArgs{
		Stream: q.stream,
		Values: map[string]any{
			"prompt": job.Prompt,
			"reply":  job.Reply,
			"vector": string(vectorJSON),
		},
	}).Err(); err != nil {
		return fmt.Errorf("enqueue cache-update job: %w", err)
	}
	return nil
}

// Run consumes jobs until ctx is canceled. A job is acknowledged only after the
// handler succeeds, so failed jobs stay pending for future retry/recovery work.
func (q *RedisStreamQueue) Run(ctx context.Context, handler func(context.Context, Job) error) {
	for {
		streams, err := q.client.XReadGroup(ctx, &redis.XReadGroupArgs{
			Group:    q.group,
			Consumer: q.consumer,
			Streams:  []string{q.stream, ">"},
			Count:    10,
			Block:    time.Second,
		}).Result()
		if errors.Is(err, redis.Nil) {
			continue
		}
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			continue
		}

		for _, stream := range streams {
			for _, message := range stream.Messages {
				job, err := jobFromMessage(message)
				if err != nil {
					_ = q.client.XAck(ctx, q.stream, q.group, message.ID).Err()
					continue
				}
				if err := handler(ctx, job); err != nil {
					continue
				}
				_ = q.client.XAck(ctx, q.stream, q.group, message.ID).Err()
			}
		}
	}
}

func jobFromMessage(message redis.XMessage) (Job, error) {
	prompt, ok := stringValue(message.Values["prompt"])
	if !ok {
		return Job{}, fmt.Errorf("cache-update job %s missing prompt", message.ID)
	}
	reply, ok := stringValue(message.Values["reply"])
	if !ok {
		return Job{}, fmt.Errorf("cache-update job %s missing reply", message.ID)
	}
	vectorRaw, ok := stringValue(message.Values["vector"])
	if !ok {
		return Job{}, fmt.Errorf("cache-update job %s missing vector", message.ID)
	}
	var vector []float64
	if err := json.Unmarshal([]byte(vectorRaw), &vector); err != nil {
		return Job{}, fmt.Errorf("decode cache-update vector: %w", err)
	}
	return Job{Prompt: prompt, Reply: reply, Vector: vector}, nil
}

func stringValue(value any) (string, bool) {
	switch v := value.(type) {
	case string:
		return v, true
	case []byte:
		return string(v), true
	default:
		return "", false
	}
}

func consumerName() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "orchestrator"
	}
	return fmt.Sprintf("%s-%d", host, os.Getpid())
}
