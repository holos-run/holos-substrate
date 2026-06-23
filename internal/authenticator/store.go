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
// Two backends could in principle declare the same spec.host; the reconciler
// registers the most recently reconciled one (last writer wins), which is the
// pragmatic choice for this phase — a host-uniqueness admission check is out of
// scope.
//
// The Store also maintains a reverse index from a Backend's object key
// (namespace/name) to the host it registered, so the reconciler can remove the
// right entry on delete: a not-found Get cannot read spec.host, but the request's
// NamespacedName is always available. DeleteByKey uses this index.
type Store struct {
	mu sync.RWMutex
	// entries maps spec.host -> resolved Entry.
	entries map[string]*Entry
	// hostByKey maps a Backend's object key (namespace/name) -> the host it most
	// recently registered, so DeleteByKey can find and remove the entry on delete.
	hostByKey map[string]string
}

// NewStore returns an empty Store ready for concurrent use.
func NewStore() *Store {
	return &Store{
		entries:   make(map[string]*Entry),
		hostByKey: make(map[string]string),
	}
}

// Set registers or replaces the Entry for entry.Host and records the reverse
// index from key (the Backend's namespace/name) to that host. When the Backend's
// host changed since its last registration, the stale host entry is removed so a
// renamed host does not leave a dangling registration.
func (s *Store) Set(key string, entry *Entry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if prevHost, ok := s.hostByKey[key]; ok && prevHost != entry.Host {
		// The Backend changed its spec.host: drop the old host's entry, but only
		// when that entry still belongs to this key (another Backend may have since
		// claimed the old host).
		delete(s.entries, prevHost)
	}
	s.entries[entry.Host] = entry
	s.hostByKey[key] = entry.Host
}

// Get returns the Entry registered for host and whether one was found. The
// returned *Entry is the stored pointer; callers must treat it as read-only.
func (s *Store) Get(host string) (*Entry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	entry, ok := s.entries[host]
	return entry, ok
}

// DeleteByKey removes the Entry a Backend registered, identified by its object
// key (namespace/name). It is idempotent — a key with no registration is a no-op
// — so the reconciler can call it unconditionally on delete or when a Backend
// goes NotReady. It resolves the host via the reverse index, so it works even
// when the caller cannot read spec.host (the not-found delete path).
func (s *Store) DeleteByKey(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	host, ok := s.hostByKey[key]
	if !ok {
		return
	}
	delete(s.entries, host)
	delete(s.hostByKey, key)
}

// Len returns the number of registered Entries. It exists for tests and
// diagnostics.
func (s *Store) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.entries)
}
