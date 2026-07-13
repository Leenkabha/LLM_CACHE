package persistence

import (
	"encoding/json"
	"testing"
)

func TestRedisStoreKeysUseStablePrefix(t *testing.T) {
	store := &RedisStore{prefix: defaultRedisPrefix}

	if got, want := store.entryKey("entry-1"), "llm_cache:entry:entry-1"; got != want {
		t.Fatalf("entryKey() = %q, want %q", got, want)
	}
	if got, want := store.idsKey(), "llm_cache:entries"; got != want {
		t.Fatalf("idsKey() = %q, want %q", got, want)
	}
}

func TestDecodeEntryClonesVector(t *testing.T) {
	original := Entry{
		ID:     "entry-1",
		Prompt: "hello",
		Reply:  "world",
		Vector: []float64{0.1, 0.2, 0.3},
	}
	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	decoded, err := decodeEntry(data)
	if err != nil {
		t.Fatalf("decodeEntry() error = %v", err)
	}
	decoded.Vector[0] = 9.9

	decodedAgain, err := decodeEntry(data)
	if err != nil {
		t.Fatalf("second decodeEntry() error = %v", err)
	}
	if decodedAgain.Vector[0] != 0.1 {
		t.Fatalf("decodeEntry() reused mutable vector backing array: got %v", decodedAgain.Vector)
	}
}
