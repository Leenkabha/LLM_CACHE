package cachequeue

import (
	"testing"

	"github.com/redis/go-redis/v9"
)

func TestJobFromMessage(t *testing.T) {
	job, err := jobFromMessage(redis.XMessage{
		ID: "1-0",
		Values: map[string]any{
			"prompt": "hello",
			"reply":  "world",
			"vector": "[0.1,0.2,0.3]",
		},
	})
	if err != nil {
		t.Fatalf("jobFromMessage() error = %v", err)
	}
	if job.Prompt != "hello" || job.Reply != "world" {
		t.Fatalf("jobFromMessage() = %+v, want prompt/reply", job)
	}
	if len(job.Vector) != 3 || job.Vector[0] != 0.1 || job.Vector[2] != 0.3 {
		t.Fatalf("jobFromMessage().Vector = %v, want [0.1 0.2 0.3]", job.Vector)
	}
}

func TestJobFromMessageRejectsInvalidVector(t *testing.T) {
	_, err := jobFromMessage(redis.XMessage{
		ID: "1-0",
		Values: map[string]any{
			"prompt": "hello",
			"reply":  "world",
			"vector": "not-json",
		},
	})
	if err == nil {
		t.Fatal("jobFromMessage() returned nil error for invalid vector")
	}
}
