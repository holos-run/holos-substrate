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

// TestStoreDeterministicOwnershipSmallestKeyWins asserts that when two Backends
// claim the same host, the lexicographically-smallest object key wins regardless
// of registration order, and Set reports win/loss accordingly.
func TestStoreDeterministicOwnershipSmallestKeyWins(t *testing.T) {
	// Order 1: smaller (ns/a) first, then larger (ns/b).
	s1 := NewStore()
	if !s1.Set("ns/a", &Entry{Host: "h", ServerURL: "https://a"}) {
		t.Fatalf("ns/a Set should win an uncontested host")
	}
	if s1.Set("ns/b", &Entry{Host: "h", ServerURL: "https://b"}) {
		t.Errorf("ns/b Set should lose to the smaller ns/a")
	}
	if entry, _ := s1.Get("h"); entry == nil || entry.ServerURL != "https://a" {
		t.Errorf("order1 Get(h) = %v, want the ns/a entry", entry)
	}

	// Order 2: larger (ns/b) first, then smaller (ns/a) seizes ownership.
	s2 := NewStore()
	if !s2.Set("ns/b", &Entry{Host: "h", ServerURL: "https://b"}) {
		t.Fatalf("ns/b Set should win an uncontested host")
	}
	if !s2.Set("ns/a", &Entry{Host: "h", ServerURL: "https://a"}) {
		t.Errorf("smaller ns/a Set should seize ownership from larger ns/b")
	}
	if entry, _ := s2.Get("h"); entry == nil || entry.ServerURL != "https://a" {
		t.Errorf("order2 Get(h) = %v, want the ns/a entry (smallest key wins)", entry)
	}

	// Both orders converge on the same owner.
	o1, _ := s1.Owner("h")
	o2, _ := s2.Owner("h")
	if o1 != o2 || o1 != "ns/a" {
		t.Errorf("ownership did not converge: order1=%q order2=%q, want both ns/a", o1, o2)
	}
}

// TestStoreLoserDeleteDoesNotClobberWinner asserts deleting the losing key never
// removes the winner's entry.
func TestStoreLoserDeleteDoesNotClobberWinner(t *testing.T) {
	s := NewStore()
	s.Set("ns/a", &Entry{Host: "h", ServerURL: "https://a"}) // winner
	s.Set("ns/b", &Entry{Host: "h", ServerURL: "https://b"}) // loser, nothing stored

	s.DeleteByKey("ns/b")
	if entry, ok := s.Get("h"); !ok || entry.ServerURL != "https://a" {
		t.Errorf("deleting loser ns/b clobbered the winner: Get(h) = %v, ok=%v", entry, ok)
	}

	s.DeleteByKey("ns/a")
	if _, ok := s.Get("h"); ok {
		t.Errorf("deleting owner ns/a left a dangling entry for h")
	}
}

// TestStoreRenameDoesNotClobberOtherOwner asserts a host rename only drops the
// old host entry when the renaming key still owns it.
func TestStoreRenameDoesNotClobberOtherOwner(t *testing.T) {
	s := NewStore()

	// ns/a owns "old".
	s.Set("ns/a", &Entry{Host: "old", ServerURL: "https://a"})
	// ns/b claims "old" too but loses (ns/a is smaller), so ns/a still owns "old".
	s.Set("ns/b", &Entry{Host: "old", ServerURL: "https://b"})
	if entry, _ := s.Get("old"); entry == nil || entry.ServerURL != "https://a" {
		t.Fatalf("ns/a should still own old; Get(old) = %v", entry)
	}
	// ns/a renames to "new". Its old-host entry is released; no other owner exists.
	s.Set("ns/a", &Entry{Host: "new", ServerURL: "https://a"})

	if _, ok := s.Get("old"); ok {
		t.Errorf("old host entry survived after its owner ns/a renamed away")
	}
	if entry, ok := s.Get("new"); !ok || entry.ServerURL != "https://a" {
		t.Errorf("ns/a new-host entry missing: Get(new) = %v, ok=%v", entry, ok)
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
