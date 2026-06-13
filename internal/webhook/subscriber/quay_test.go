package subscriber

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/holos-run/holos-paas/internal/task"
)

// TestQuayParseGolden decodes the captured Quay repo_push payload and asserts
// the resulting tasks field-by-field. The payload lists two tags, so the
// one-task-per-tag fan-out (ADR-13) must yield two tasks. receivedAt is
// asserted only against the injected fixed clock.
func TestQuayParseGolden(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("testdata", "quay-repo-push.json"))
	if err != nil {
		t.Fatalf("reading testdata: %v", err)
	}
	receivedAt := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)

	got, err := QuayParser{}.Parse(QuaySource, nil, body, receivedAt)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	want := []*task.DeployTask{
		task.NewDeployTask(QuaySource, "holos/sample-app", "v2", "quay.example.com/holos/sample-app", receivedAt),
		task.NewDeployTask(QuaySource, "holos/sample-app", "latest", "quay.example.com/holos/sample-app", receivedAt),
	}

	if len(got) != len(want) {
		t.Fatalf("got %d tasks, want %d", len(got), len(want))
	}
	for i := range want {
		if !proto.Equal(got[i], want[i]) {
			t.Errorf("task[%d] mismatch:\n got = %#v\nwant = %#v", i, got[i], want[i])
		}
		// Spot-check the derived/stamped fields explicitly for clarity.
		if got[i].GetApplication().GetName() != "sample-app" {
			t.Errorf("task[%d].Application.Name = %q, want %q", i, got[i].GetApplication().GetName(), "sample-app")
		}
		if got[i].GetSource() != QuaySource {
			t.Errorf("task[%d].Source = %q, want %q", i, got[i].GetSource(), QuaySource)
		}
		if got[i].GetSchemaVersion() != task.SchemaVersion {
			t.Errorf("task[%d].SchemaVersion = %d, want %d", i, got[i].GetSchemaVersion(), task.SchemaVersion)
		}
		if got[i].GetDigest() != "" {
			t.Errorf("task[%d].Digest = %q, want empty (deferred)", i, got[i].GetDigest())
		}
		if !got[i].GetReceivedAt().AsTime().Equal(receivedAt) {
			t.Errorf("task[%d].ReceivedAt = %v, want %v", i, got[i].GetReceivedAt().AsTime(), receivedAt)
		}
	}
	if got[0].GetIdempotencyKey() == got[1].GetIdempotencyKey() {
		t.Error("tasks for distinct tags share an idempotency key")
	}
}

// TestQuayParseFallbackRepository verifies the namespace/name fallback when the
// payload omits the explicit "repository" field.
func TestQuayParseFallbackRepository(t *testing.T) {
	body := []byte(`{"namespace":"holos","name":"sample-app","docker_url":"u","updated_tags":["v1"]}`)
	got, err := QuayParser{}.Parse(QuaySource, nil, body, time.Now())
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d tasks, want 1", len(got))
	}
	if got[0].GetRepository() != "holos/sample-app" {
		t.Errorf("Repository = %q, want %q", got[0].GetRepository(), "holos/sample-app")
	}
	if got[0].GetApplication().GetName() != "sample-app" {
		t.Errorf("Application.Name = %q, want %q", got[0].GetApplication().GetName(), "sample-app")
	}
}

func TestQuayParseMalformed(t *testing.T) {
	cases := map[string][]byte{
		"truncated json":     []byte("{"),
		"empty object":       []byte("{}"),
		"json array":         []byte(`["v1","v2"]`),
		"no updated_tags":    []byte(`{"repository":"holos/sample-app"}`),
		"empty updated_tags": []byte(`{"repository":"holos/sample-app","updated_tags":[]}`),
		"empty tag":          []byte(`{"repository":"holos/sample-app","updated_tags":[""]}`),
		"empty app segment":  []byte(`{"repository":"holos/","updated_tags":["v1"]}`),
		"empty body":         []byte(""),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := QuayParser{}.Parse(QuaySource, nil, body, time.Now())
			if err == nil {
				t.Fatalf("expected error, got nil (tasks=%#v)", got)
			}
			if len(got) != 0 {
				t.Errorf("expected zero tasks on error, got %d", len(got))
			}
			if err.Error() == "" {
				t.Error("error message is empty; want descriptive error")
			}
		})
	}
}

func TestRegistryLookup(t *testing.T) {
	r := DefaultRegistry()

	p, found := r.Lookup(QuaySource)
	if !found {
		t.Fatalf("quay parser not registered in DefaultRegistry")
	}
	if _, ok := p.(QuayParser); !ok {
		t.Errorf("quay source resolved to %T, want QuayParser", p)
	}

	if _, found := r.Lookup("github"); found {
		t.Error("unknown source reported as found; Phase 2 cannot Term it")
	}
}

func TestRegistryRegisterReplaces(t *testing.T) {
	r := NewRegistry()
	r.Register("x", QuayParser{})
	r.Register("x", QuayParser{})
	if _, found := r.Lookup("x"); !found {
		t.Error("re-registered source not found")
	}
}
