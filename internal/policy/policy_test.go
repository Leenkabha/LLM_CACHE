package policy

import "testing"

func TestManagerDefaultsInvalidPolicyToLRU(t *testing.T) {
	manager := New("unknown")
	if got := manager.Current(); got != "lru" {
		t.Fatalf("Current() = %q, want lru", got)
	}
}

func TestManagerSetRejectsRuntimeSwitch(t *testing.T) {
	manager := New("lru")

	if err := manager.Set("lru"); err != nil {
		t.Fatalf("Set() with current policy error = %v", err)
	}

	if err := manager.Set("lfu"); err == nil {
		t.Fatal("Set() with different policy returned nil, want runtime-switch error")
	}
	if got := manager.Current(); got != "lru" {
		t.Fatalf("Current() after rejected switch = %q, want lru", got)
	}
}

func TestManagerSetRejectsInvalidPolicy(t *testing.T) {
	manager := New("lru")
	if err := manager.Set("random"); err == nil {
		t.Fatal("Set() with invalid policy returned nil, want error")
	}
}

func TestManagerLRUTracksRecency(t *testing.T) {
	manager := New("lru")

	manager.OnInsert("entry-1")
	manager.OnInsert("entry-2")
	manager.OnInsert("entry-3")
	manager.OnHit("entry-1")

	if victim, ok := manager.Victim(); !ok || victim != "entry-2" {
		t.Fatalf("Victim() = (%q, %v), want (entry-2, true)", victim, ok)
	}

	manager.OnDelete("entry-1")
	manager.OnDelete("entry-2")

	if victim, ok := manager.Victim(); !ok || victim != "entry-3" {
		t.Fatalf("Victim() after deletes = (%q, %v), want (entry-3, true)", victim, ok)
	}

	manager.Flush()
	if victim, ok := manager.Victim(); ok {
		t.Fatalf("Victim() after Flush() = (%q, true), want no victim", victim)
	}
	if got := manager.Current(); got != "lru" {
		t.Fatalf("Current() after hooks = %q, want lru", got)
	}
}

func TestManagerLFUTracksFrequency(t *testing.T) {
	manager := New("lfu")

	manager.OnInsert("entry-1")
	manager.OnInsert("entry-2")
	manager.OnInsert("entry-3")
	manager.OnHit("entry-1")
	manager.OnHit("entry-1")
	manager.OnHit("entry-2")

	if victim, ok := manager.Victim(); !ok || victim != "entry-3" {
		t.Fatalf("Victim() = (%q, %v), want (entry-3, true)", victim, ok)
	}

	manager.OnDelete("entry-3")
	if victim, ok := manager.Victim(); !ok || victim != "entry-2" {
		t.Fatalf("Victim() after delete = (%q, %v), want (entry-2, true)", victim, ok)
	}
}

func TestManagerLFUTieBreaksByOldestInsert(t *testing.T) {
	manager := New("lfu")

	manager.OnInsert("entry-1")
	manager.OnInsert("entry-2")

	if victim, ok := manager.Victim(); !ok || victim != "entry-1" {
		t.Fatalf("Victim() = (%q, %v), want oldest equal-frequency entry-1", victim, ok)
	}
}
