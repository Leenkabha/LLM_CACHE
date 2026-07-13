package persistence

import (
	"testing"
	"time"
)

func TestMemoryStoreSaveLoadListSizeDeleteFlush(t *testing.T) {
	store := NewMemoryStore()
	createdAt := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)

	entry := Entry{
		ID:        "entry-1",
		Prompt:    "What is virtual memory?",
		Reply:     "Virtual memory is an abstraction over physical memory.",
		Vector:    []float64{0.1, 0.2, 0.3},
		CreatedAt: createdAt,
	}

	if err := store.Save(entry); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	size, err := store.Size()
	if err != nil {
		t.Fatalf("Size() error = %v", err)
	}
	if size != 1 {
		t.Fatalf("Size() = %d, want 1", size)
	}

	loaded, ok := store.Load(entry.ID)
	if !ok {
		t.Fatalf("Load(%q) returned ok=false", entry.ID)
	}
	if loaded.ID != entry.ID || loaded.Prompt != entry.Prompt || loaded.Reply != entry.Reply || !loaded.CreatedAt.Equal(createdAt) {
		t.Fatalf("Load() = %+v, want %+v", loaded, entry)
	}
	if len(loaded.Vector) != len(entry.Vector) {
		t.Fatalf("Load().Vector length = %d, want %d", len(loaded.Vector), len(entry.Vector))
	}

	entries, err := store.List()
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(entries) != 1 || entries[0].ID != entry.ID {
		t.Fatalf("List() = %+v, want one entry with ID %q", entries, entry.ID)
	}

	if err := store.Delete(entry.ID); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if _, ok := store.Load(entry.ID); ok {
		t.Fatalf("Load(%q) after Delete() returned ok=true", entry.ID)
	}

	if err := store.Save(entry); err != nil {
		t.Fatalf("Save() after Delete() error = %v", err)
	}
	if err := store.Flush(); err != nil {
		t.Fatalf("Flush() error = %v", err)
	}
	size, err = store.Size()
	if err != nil {
		t.Fatalf("Size() after Flush() error = %v", err)
	}
	if size != 0 {
		t.Fatalf("Size() after Flush() = %d, want 0", size)
	}
}

func TestMemoryStoreClonesVectors(t *testing.T) {
	store := NewMemoryStore()
	vector := []float64{0.1, 0.2, 0.3}

	if err := store.Save(Entry{ID: "entry-1", Vector: vector}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	vector[0] = 9.9

	loaded, ok := store.Load("entry-1")
	if !ok {
		t.Fatal("Load() returned ok=false")
	}
	if loaded.Vector[0] != 0.1 {
		t.Fatalf("stored vector was mutated through original slice: got %v", loaded.Vector)
	}

	loaded.Vector[1] = 8.8
	loadedAgain, ok := store.Load("entry-1")
	if !ok {
		t.Fatal("second Load() returned ok=false")
	}
	if loadedAgain.Vector[1] != 0.2 {
		t.Fatalf("stored vector was mutated through loaded slice: got %v", loadedAgain.Vector)
	}

	entries, err := store.List()
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	entries[0].Vector[2] = 7.7

	loadedAgain, ok = store.Load("entry-1")
	if !ok {
		t.Fatal("third Load() returned ok=false")
	}
	if loadedAgain.Vector[2] != 0.3 {
		t.Fatalf("stored vector was mutated through listed slice: got %v", loadedAgain.Vector)
	}
}
