package authenticator

import (
	"sync"

	authenticatorv1alpha1 "github.com/holos-run/holos-paas/api/authenticator/v1alpha1"
)

// Entry is the resolved, ready-to-serve configuration for a single Backend,
// keyed by its spec.host. The BackendReconciler builds it once OIDC discovery
// succeeds and the group-mapping CEL expression compiles, then registers it in
// the Store; the gRPC Check path (HOL-1388) looks it up by the request's
// :authority/Host and uses it to validate the bearer token and resolve the
// Kubernetes identity.
//
// An Entry is treated as immutable once stored: a Backend update replaces the
// whole Entry rather than mutating fields in place, so the gRPC path never reads
// a half-updated value. The compiled CEL program and the OIDC verifier it holds
// are themselves safe for concurrent use.
type Entry struct {
	// Host is the spec.host this Entry is keyed by (the request :authority/Host
	// the Backend matches). It is duplicated here for convenience and logging.
	Host string

	// Authenticator validates a raw bearer token against this backend's OIDC
	// client and maps the verified claims to a Kubernetes username and groups.
	Authenticator *Authenticator

	// UsernameClaim is the token claim the username is read from (spec.oidc.usernameClaim).
	UsernameClaim string

	// ServerURL is the upstream Kubernetes API server URL the Check path forwards
	// authenticated requests to (spec.server.url). Recorded here for the Check
	// path; this phase does not dial it.
	ServerURL string

	// ServerCABundle is the optional PEM CA bundle trusted when reaching the
	// upstream API server (spec.server.caBundle). Empty means system trust.
	ServerCABundle []byte

	// CredentialsSecretRef names the Secret holding the backend's privileged
	// impersonator credential. The Check path (HOL-1388) resolves it; this phase
	// only records the ref so the Check path need not re-read the Backend CR.
	CredentialsSecretRef authenticatorv1alpha1.SecretReference
}

// Store is a concurrency-safe registry of ready Backends keyed by spec.host. The
// BackendReconciler is the sole writer (Set/Delete as Backends become ready,
// change, or are removed); the gRPC Check Runnable is a reader (Get by host). It
// lives in internal/authenticator so both depend on it without an import cycle.
//
// Two backends could in principle declare the same spec.host. Because this
// reconciler runs on every replica (each maintains its own process-local Store),
// host ownership must be **order-independent** so all replicas converge on the
// same owner regardless of the sequence in which they observe the Backends and
// regardless of restarts. The Store therefore resolves a host collision
// deterministically: the lexicographically-smallest owning object key wins. A
// later Set by a larger key does not seize a host a smaller key already owns, and
// a Set by a smaller key takes ownership from a larger one. (A full API-backed
// uniqueness/admission mechanism remains a documented follow-up; this convergent
// rule removes the cross-replica nondeterminism without one.)
//
// The Store also maintains a reverse index from a Backend's object key
// (namespace/name) to the host it registered, so the reconciler can remove the
// right entry on delete: a not-found Get cannot read spec.host, but the request's
// NamespacedName is always available. DeleteByKey uses this index.
type Store struct {
	mu sync.RWMutex
	// entries maps spec.host -> resolved Entry.
	entries map[string]*Entry
	// ownerByHost maps spec.host -> the object key of the Backend that owns that
	// host entry (the lexicographically-smallest key claiming it), so a delete or a
	// losing Set is a no-op rather than clobbering the deterministic winner.
	ownerByHost map[string]string
	// hostByKey maps a Backend's object key (namespace/name) -> the host it most
	// recently registered, so DeleteByKey can find and remove the entry on delete.
	hostByKey map[string]string
}

// NewStore returns an empty Store ready for concurrent use.
func NewStore() *Store {
	return &Store{
		entries:     make(map[string]*Entry),
		ownerByHost: make(map[string]string),
		hostByKey:   make(map[string]string),
	}
}

// Set registers or replaces the Entry for entry.Host, recording key (the
// Backend's namespace/name) as the host's current owner and the reverse index
// from key to that host. When the Backend changed its spec.host since its last
// registration, the previous host's entry is removed only when this key still
// owns it — a different Backend may have claimed the old host in the interim, and
// clobbering its entry would silently break that backend.
// Set returns true when key won (or retained) ownership of entry.Host and the
// entry was stored, and false when a lexicographically-smaller key already owns
// the host (this key lost the deterministic tie-break and nothing was stored for
// it). The reconciler uses the return to decide whether the Backend is Ready
// (owner) or in HostConflict (loser), order-independently across replicas.
func (s *Store) Set(key string, entry *Entry) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Resolve ownership deterministically: a smaller current owner wins.
	if owner, ok := s.ownerByHost[entry.Host]; ok && owner != key && owner < key {
		// A smaller key already owns this host; this key loses. Do not store its
		// entry. Also clear any stale reverse-index row pointing this key at this
		// host (e.g. it previously owned the host and a smaller key has since taken
		// it), so the index does not claim ownership the entry table contradicts.
		if s.hostByKey[key] == entry.Host {
			delete(s.hostByKey, key)
		}
		return false
	}

	if prevHost, ok := s.hostByKey[key]; ok && prevHost != entry.Host {
		// Host rename: drop the old host's entry only if this key still owns it.
		if s.ownerByHost[prevHost] == key {
			delete(s.entries, prevHost)
			delete(s.ownerByHost, prevHost)
		}
	}
	s.entries[entry.Host] = entry
	s.ownerByHost[entry.Host] = key
	s.hostByKey[key] = entry.Host
	return true
}

// Get returns the Entry registered for host and whether one was found. The
// returned *Entry is the stored pointer; callers must treat it as read-only.
func (s *Store) Get(host string) (*Entry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	entry, ok := s.entries[host]
	return entry, ok
}

// Owner returns the object key currently owning host and whether host is
// registered. The reconciler uses it to detect a host collision — a host already
// owned by a different Backend — and reject the new claimant deterministically
// rather than silently overwriting the data-path routing for that host.
func (s *Store) Owner(host string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	owner, ok := s.ownerByHost[host]
	return owner, ok
}

// DeleteByKey removes the Entry a Backend registered, identified by its object
// key (namespace/name). It is idempotent — a key with no registration is a no-op
// — so the reconciler can call it unconditionally on delete or when a Backend
// goes NotReady. It resolves the host via the reverse index, so it works even
// when the caller cannot read spec.host (the not-found delete path).
//
// The host entry is removed only when this key still owns it: if a different
// Backend has since claimed the same host (last-writer-wins), deleting the stale
// key must not clobber the current winner's entry. The key's own reverse-index
// row is always cleared.
func (s *Store) DeleteByKey(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	host, ok := s.hostByKey[key]
	if !ok {
		return
	}
	delete(s.hostByKey, key)
	if s.ownerByHost[host] == key {
		delete(s.entries, host)
		delete(s.ownerByHost, host)
	}
}

// Len returns the number of registered Entries. It exists for tests and
// diagnostics.
func (s *Store) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.entries)
}
