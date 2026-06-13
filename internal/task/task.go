// Package task defines the DeployTask message: the well-known, versioned
// contract between the webhook subscriber (ADR-10) and the deployer (ADR-11).
//
// The subscriber parses a source webhook event (e.g. a Quay repository push)
// into one or more DeployTasks and publishes each to the [DeploySubject]
// subject on JetStream as binary protobuf (ADR-14); the deployer consumes them
// and performs a single, idempotent KRM write. This package is the boundary
// between the two: it owns the [DeploySubject], the [SchemaVersion], and the
// helpers that stamp and key a task, deliberately free of any NATS or
// Kubernetes import so the contract and its helpers stay trivially testable and
// reusable by the future deployer.
//
// Per ADR-14 the wire schema itself lives in
// proto/holos/paas/pipeline/v1alpha1/pipeline.proto and the [DeployTask] Go
// type is generated from it — never hand-edited. Changing the message shape —
// adding, removing, or repurposing a field, or changing the meaning of the
// idempotency key — is an ADR-level change: edit the .proto, bump
// [SchemaVersion], and revise ADR-10/ADR-11/ADR-14 rather than altering the
// contract silently. Consumers reading an unknown future SchemaVersion should
// fail closed.
package task

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	pipelinev1alpha1 "github.com/holos-run/holos-paas/internal/gen/holos/paas/pipeline/v1alpha1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// DeployTask is the generated protobuf deploy instruction (ADR-14). It is an
// alias for the generated type so callers refer to it as task.DeployTask while
// the schema stays defined in the .proto. Construct one through [NewDeployTask]
// (or a source parser) so the schema version, derived app, and idempotency key
// are populated consistently.
type DeployTask = pipelinev1alpha1.DeployTask

// ApplicationRef is the generated Application identity carried by a task,
// re-exported for callers that build or inspect a task's application field.
type ApplicationRef = pipelinev1alpha1.ApplicationRef

// SchemaVersion is the version of the DeployTask wire contract. It is stamped
// into every task's schema_version field so consumers can detect a contract
// change and fail closed on an unknown version. Bump it whenever the DeployTask
// shape or field semantics change (an ADR-level change per ADR-10/ADR-11/
// ADR-14, editing the .proto in lock-step).
const SchemaVersion = 1

// DeploySubject is the JetStream subject the subscriber publishes DeployTasks
// to and the deployer consumes from (ADR-10). It is part of the contract.
const DeploySubject = "tasks.deploy"

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
// [SchemaVersion], deriving the application name from repository (into
// Application.Name), and computing the stable [IdempotencyKey]. receivedAt is
// recorded verbatim as a protobuf timestamp. This is the single constructor
// source parsers should use so every task is built consistently.
func NewDeployTask(source, repository, tag, dockerURL string, receivedAt time.Time) *DeployTask {
	return &pipelinev1alpha1.DeployTask{
		SchemaVersion:  SchemaVersion,
		IdempotencyKey: IdempotencyKey(source, repository, tag, dockerURL),
		Application: &pipelinev1alpha1.ApplicationRef{
			Name: AppFromRepository(repository),
		},
		Repository: repository,
		Tag:        tag,
		Source:     source,
		ReceivedAt: timestamppb.New(receivedAt),
	}
}
