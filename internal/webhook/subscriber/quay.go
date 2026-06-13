package subscriber

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/holos-run/holos-paas/internal/task"
)

// QuaySource is the webhook source token for Quay repository-push events.
const QuaySource = "quay"

// quayRepoPush models the fields of the Quay repository-push ("repo_push")
// notification payload this parser consumes. See the Red Hat Quay
// repository-events documentation referenced in ADR-13:
//
//	https://docs.redhat.com/en/documentation/red_hat_quay/2.9/html-single/use_red_hat_quay/index#repository-events
//
// The documented payload carries the pushed tags in updated_tags but no
// manifest digest (ADR-13); digest resolution is deferred. Fields not needed
// to build a DeployTask are ignored.
type quayRepoPush struct {
	// Repository is the "namespace/name" repository path, e.g.
	// "holos/sample-app". Quay sends this in the "repository" field.
	Repository string `json:"repository"`
	// Namespace is the owning namespace, e.g. "holos".
	Namespace string `json:"namespace"`
	// Name is the repository's short name, e.g. "sample-app".
	Name string `json:"name"`
	// DockerURL is the registry-qualified repository URL, e.g.
	// "quay.example.com/holos/sample-app"; it qualifies the idempotency key.
	DockerURL string `json:"docker_url"`
	// Homepage is the Quay UI URL for the repository (informational).
	Homepage string `json:"homepage"`
	// UpdatedTags lists the tags pushed in this event; one DeployTask is
	// emitted per entry (ADR-13, "one task per pushed tag").
	UpdatedTags []string `json:"updated_tags"`
}

// QuayParser parses Quay repository-push notifications into DeployTasks. It
// holds no state and is safe for concurrent use.
type QuayParser struct{}

// Ensure QuayParser satisfies the Parser interface.
var _ Parser = QuayParser{}

// repository returns the best repository path the payload provides, preferring
// the explicit "repository" field and falling back to "namespace/name".
func (p quayRepoPush) repository() string {
	if p.Repository != "" {
		return p.Repository
	}
	if p.Namespace != "" && p.Name != "" {
		return p.Namespace + "/" + p.Name
	}
	if p.Name != "" {
		return p.Name
	}
	return ""
}

// Parse decodes a Quay repository-push body into one DeployTask per tag in
// updated_tags (ADR-13). source is stamped onto each task (normally
// [QuaySource]) and receivedAt is copied verbatim. hdr is currently unused —
// Quay carries everything needed in the body — but is accepted to satisfy the
// [Parser] contract and for parity with header-driven sources.
//
// A body that is not a JSON object, that omits a usable repository, or that
// carries no updated_tags returns a non-nil, descriptive error and no tasks.
// The Term-vs-Nak decision for such errors lives in Phase 2 (ADR-13); this
// parser only guarantees a clean typed error.
func (QuayParser) Parse(source string, hdr http.Header, body []byte, receivedAt time.Time) ([]task.DeployTask, error) {
	_ = hdr // Quay needs no headers to parse; accepted for the Parser contract.

	var payload quayRepoPush
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("parsing quay repo_push payload: %w", err)
	}

	repository := payload.repository()
	if repository == "" {
		return nil, fmt.Errorf("parsing quay repo_push payload: missing repository (no repository, namespace/name, or name field)")
	}
	// The DeployTask contract requires a non-empty app, derived mechanically
	// from the repository's last path segment. A repository like "holos/"
	// (trailing slash, empty final segment) is malformed: it would yield an
	// empty app. Reject it here rather than emit an invalid task.
	if task.AppFromRepository(repository) == "" {
		return nil, fmt.Errorf("parsing quay repo_push payload: repository %q has an empty final path segment; cannot derive app", repository)
	}
	if len(payload.UpdatedTags) == 0 {
		return nil, fmt.Errorf("parsing quay repo_push payload for repository %q: no updated_tags", repository)
	}

	tasks := make([]task.DeployTask, 0, len(payload.UpdatedTags))
	for _, tag := range payload.UpdatedTags {
		if tag == "" {
			return nil, fmt.Errorf("parsing quay repo_push payload for repository %q: empty tag in updated_tags", repository)
		}
		tasks = append(tasks, task.NewDeployTask(source, repository, tag, payload.DockerURL, receivedAt))
	}
	return tasks, nil
}
