// Package task defines the DeployTask message: the well-known, versioned
// contract between the webhook subscriber (ADR-10) and the deployer (ADR-11).
//
// The subscriber parses a source webhook event (e.g. a Quay repository push)
// into one or more DeployTasks and publishes each to the [DeploySubject]
// subject on JetStream; the deployer consumes them and performs a single,
// idempotent KRM write. This package is the boundary between the two: it is
// deliberately free of any NATS or Kubernetes import so the contract and its
// helpers stay trivially testable and reusable by the future deployer.
//
// This struct is a stable wire contract. Changing its shape — adding,
// removing, or repurposing a field, or changing the meaning of the
// idempotency key — is an ADR-level change: bump [SchemaVersion] and revise
// ADR-10/ADR-11 rather than altering the contract silently. Consumers reading
// an unknown future SchemaVersion should fail closed.
package task

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// SchemaVersion is the version of the DeployTask wire contract. It is embedded
// in every marshaled message as the "schemaVersion" field so consumers can
// detect a contract change and fail closed on an unknown version. Bump it
// whenever the DeployTask shape or field semantics change (an ADR-level
// change per ADR-10/ADR-11).
const SchemaVersion = 1

// DeploySubject is the JetStream subject the subscriber publishes DeployTasks
// to and the deployer consumes from (ADR-10). It is part of the contract.
const DeploySubject = "tasks.deploy"

// DeployTask is a single, fully-resolved instruction to deploy one image tag.
// One source event (e.g. a Quay push listing several tags) fans out to one
// DeployTask per tag (ADR-13, "one task per pushed tag").
//
// The zero value is not a valid task; construct tasks through a source parser
// (see internal/webhook/subscriber) so the idempotency key and schema version
// are populated consistently.
type DeployTask struct {
	// SchemaVersion is the contract version; always [SchemaVersion] on
	// freshly constructed tasks. Present so a consumer can reject an unknown
	// future version.
	SchemaVersion int `json:"schemaVersion"`

	// IdempotencyKey is a stable, deterministic key derived from the source
	// event (see [IdempotencyKey]). A redelivered raw event yields a
	// byte-identical key so the deployer can deduplicate without coordinating
	// state. It contains no timestamp or random component.
	IdempotencyKey string `json:"idempotencyKey"`

	// App is the application name derived mechanically from the repository
	// (see [AppFromRepository]); it is the last path segment of the
	// repository. This is a derivation only — no Application KRM lookup
	// happens here; matching a task to an Application (and disambiguating a
	// config repository such as "sample-app-config" from its owning
	// Application) is deferred KRM-matching work (ADR-13, HOL-1201 scope).
	// The deferred matcher may overwrite App with the matched Application
	// identity without a contract break.
	App string `json:"app"`

	// Repository is the source repository the image was pushed to, e.g.
	// "holos/sample-app".
	Repository string `json:"repository"`

	// Tag is the single image tag this task deploys, e.g. "v2".
	Tag string `json:"tag"`

	// Digest is the resolved manifest digest, when known. Quay's documented
	// repo_push payload carries no digest (ADR-13), and registry digest
	// resolution is deferred (HOL-1201 scope); the field is present and
	// omitempty so the deferred resolution work can populate it without a
	// contract break.
	Digest string `json:"digest,omitempty"`

	// Source is the webhook source token the event arrived from, e.g. "quay".
	Source string `json:"source"`

	// ReceivedAt is the wall-clock time the subscriber observed the source
	// event. It is informational/observability only and deliberately excluded
	// from the idempotency key so redeliveries remain byte-identical.
	ReceivedAt time.Time `json:"receivedAt"`
}

// IdempotencyKey derives the stable idempotency key for a deploy of tag in
// repository from source, optionally qualified by dockerURL. The key is the
// hex-encoded SHA-256 of the fields and contains no timestamp or random value,
// so a redelivered raw event always produces the identical key (ADR-13
// idempotency).
//
// The hash input is unambiguously framed: each field is length-prefixed (its
// byte length as a decimal followed by ":") before concatenation. Unlike a
// plain delimiter join, this is injective even when a field contains the
// delimiter or NUL/control bytes, so two distinct field tuples can never
// produce the same preimage (e.g. tag "v1"+dockerURL "\x00x" is distinct from
// tag "v1\x00"+dockerURL "x").
//
// dockerURL may be empty; when present it disambiguates repositories that
// share a name across registries. Construct tasks with [NewDeployTask] rather
// than setting the key by hand so every task in the system computes the key
// the same way.
func IdempotencyKey(source, repository, tag, dockerURL string) string {
	var b strings.Builder
	for _, field := range []string{source, repository, tag, dockerURL} {
		fmt.Fprintf(&b, "%d:%s", len(field), field)
	}
	h := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(h[:])
}

// AppFromRepository derives the application name from a repository path. It is
// the last "/"-separated segment of the repository, so "holos/sample-app"
// yields "sample-app" and a bare "sample-app" yields "sample-app". The
// derivation is intentionally mechanical: it performs no Application KRM lookup
// (that matching is deferred work per ADR-13).
func AppFromRepository(repository string) string {
	if i := strings.LastIndex(repository, "/"); i >= 0 {
		return repository[i+1:]
	}
	return repository
}

// NewDeployTask constructs a DeployTask for a single tag, stamping the current
// [SchemaVersion], deriving [App] from repository, and computing the stable
// [IdempotencyKey]. receivedAt is recorded verbatim. This is the single
// constructor source parsers should use so every task is built consistently.
func NewDeployTask(source, repository, tag, dockerURL string, receivedAt time.Time) DeployTask {
	return DeployTask{
		SchemaVersion:  SchemaVersion,
		IdempotencyKey: IdempotencyKey(source, repository, tag, dockerURL),
		App:            AppFromRepository(repository),
		Repository:     repository,
		Tag:            tag,
		Source:         source,
		ReceivedAt:     receivedAt,
	}
}
