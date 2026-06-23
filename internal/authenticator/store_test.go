package authenticator

import (
	"sync"
	"testing"
)

// TestStoreSetGetDelete covers the basic registry lifecycle keyed by object key
// and host.
func TestStoreSetGetDelete(t *testing.T) {
	s := NewStore()

	if _, ok := s.Get("api.example.com"); ok {
		t.Fatalf("Get on empty store reported found")
	}

	s.Set("ns/backend-a", &Entry{Host: "api.example.com", ServerURL: "https://api.example.com"})
	entry, ok := s.Get("api.example.com")
	if !ok {
		t.Fatalf("Get after Set reported not found")
	}
	if entry.ServerURL != "https://api.example.com" {
		t.Errorf("ServerURL = %q, want https://api.example.com", entry.ServerURL)
	}
	if s.Len() != 1 {
		t.Errorf("Len = %d, want 1", s.Len())
	}

	s.DeleteByKey("ns/backend-a")
	if _, ok := s.Get("api.example.com"); ok {
		t.Fatalf("Get after DeleteByKey reported found")
	}
	if s.Len() != 0 {
		t.Errorf("Len after delete = %d, want 0", s.Len())
	}
}

// TestStoreDeleteByKeyIdempotent asserts deleting an unknown key is a no-op.
func TestStoreDeleteByKeyIdempotent(t *testing.T) {
	s := NewStore()
	s.DeleteByKey("ns/missing") // must not panic
	s.Set("ns/a", &Entry{Host: "h"})
	s.DeleteByKey("ns/a")
	s.DeleteByKey("ns/a") // second delete is a no-op
	if s.Len() != 0 {
		t.Errorf("Len = %d, want 0", s.Len())
	}
}

// TestStoreHostRenameDropsStaleEntry asserts that when a Backend changes its
// spec.host, re-registering under the same object key removes the old host's
// entry so no dangling registration survives.
func TestStoreHostRenameDropsStaleEntry(t *testing.T) {
	s := NewStore()
	s.Set("ns/backend", &Entry{Host: "old.example.com"})
	s.Set("ns/backend", &Entry{Host: "new.example.com"})

	if _, ok := s.Get("old.example.com"); ok {
		t.Errorf("old host entry survived a host rename")
	}
	if _, ok := s.Get("new.example.com"); !ok {
		t.Errorf("new host entry missing after rename")
	}
	if s.Len() != 1 {
		t.Errorf("Len = %d, want 1 after rename", s.Len())
	}

	// Deleting by key removes the current (new) host entry.
	s.DeleteByKey("ns/backend")
	if s.Len() != 0 {
		t.Errorf("Len = %d, want 0 after delete", s.Len())
	}
}

// TestStoreDuplicateHostOwnership asserts that when two Backends claim the same
// host (last-writer-wins) and the stale (losing) Backend is later deleted, the
// winner's entry is preserved rather than clobbered.
func TestStoreDuplicateHostOwnership(t *testing.T) {
	s := NewStore()

	// Backend A claims host h.
	s.Set("ns/a", &Entry{Host: "h", ServerURL: "https://a"})
	// Backend B claims the same host h (last writer wins).
	s.Set("ns/b", &Entry{Host: "h", ServerURL: "https://b"})

	if entry, _ := s.Get("h"); entry == nil || entry.ServerURL != "https://b" {
		t.Fatalf("after B claims h, Get(h) = %v, want the B entry", entry)
	}

	// Deleting the stale A must NOT remove B's entry for h.
	s.DeleteByKey("ns/a")
	entry, ok := s.Get("h")
	if !ok || entry.ServerURL != "https://b" {
		t.Errorf("deleting stale A clobbered the winner: Get(h) = %v, ok=%v, want B entry", entry, ok)
	}

	// Deleting the owner B finally removes the entry.
	s.DeleteByKey("ns/b")
	if _, ok := s.Get("h"); ok {
		t.Errorf("deleting owner B left a dangling entry for h")
	}
}

// TestStoreRenameDoesNotClobberOtherOwner asserts a host rename only drops the
// old host entry when the renaming key still owns it.
func TestStoreRenameDoesNotClobberOtherOwner(t *testing.T) {
	s := NewStore()

	// A owns "old".
	s.Set("ns/a", &Entry{Host: "old", ServerURL: "https://a"})
	// B claims "old" too (B now owns "old").
	s.Set("ns/b", &Entry{Host: "old", ServerURL: "https://b"})
	// A renames to "new". Since A no longer owns "old", B's "old" entry survives.
	s.Set("ns/a", &Entry{Host: "new", ServerURL: "https://a"})

	if entry, ok := s.Get("old"); !ok || entry.ServerURL != "https://b" {
		t.Errorf("A's rename clobbered B's old-host entry: Get(old) = %v, ok=%v", entry, ok)
	}
	if entry, ok := s.Get("new"); !ok || entry.ServerURL != "https://a" {
		t.Errorf("A's new-host entry missing: Get(new) = %v, ok=%v", entry, ok)
	}
}

// TestStoreConcurrentAccess runs concurrent writers and readers under the race
// detector to confirm the Store is concurrency-safe.
func TestStoreConcurrentAccess(t *testing.T) {
	s := NewStore()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func(n int) {
			defer wg.Done()
			s.Set("ns/backend", &Entry{Host: "h"})
		}(i)
		go func() {
			defer wg.Done()
			_, _ = s.Get("h")
			_ = s.Len()
		}()
	}
	wg.Wait()
}
