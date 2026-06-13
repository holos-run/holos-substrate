package task

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestNewDeployTaskFields(t *testing.T) {
	receivedAt := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
	got := NewDeployTask("quay", "holos/sample-app", "v2", "quay.example.com/holos/sample-app", receivedAt)

	if got.SchemaVersion != SchemaVersion {
		t.Errorf("SchemaVersion = %d, want %d", got.SchemaVersion, SchemaVersion)
	}
	if got.App != "sample-app" {
		t.Errorf("App = %q, want %q", got.App, "sample-app")
	}
	if got.Repository != "holos/sample-app" {
		t.Errorf("Repository = %q, want %q", got.Repository, "holos/sample-app")
	}
	if got.Tag != "v2" {
		t.Errorf("Tag = %q, want %q", got.Tag, "v2")
	}
	if got.Source != "quay" {
		t.Errorf("Source = %q, want %q", got.Source, "quay")
	}
	if got.Digest != "" {
		t.Errorf("Digest = %q, want empty", got.Digest)
	}
	if !got.ReceivedAt.Equal(receivedAt) {
		t.Errorf("ReceivedAt = %v, want %v", got.ReceivedAt, receivedAt)
	}
	if got.IdempotencyKey == "" {
		t.Error("IdempotencyKey is empty")
	}
}

func TestDeployTaskJSONRoundTrip(t *testing.T) {
	receivedAt := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
	want := NewDeployTask("quay", "holos/sample-app", "v2", "", receivedAt)

	b, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var got DeployTask
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got != want {
		t.Errorf("round-trip mismatch:\n got = %#v\nwant = %#v", got, want)
	}
}

// TestDeployTaskJSONShape pins the marshaled field names that form the wire
// contract, including the embedded schema version and the digest omitempty.
func TestDeployTaskJSONShape(t *testing.T) {
	receivedAt := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
	b, err := json.Marshal(NewDeployTask("quay", "holos/sample-app", "v2", "", receivedAt))
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	for _, key := range []string{"schemaVersion", "idempotencyKey", "app", "repository", "tag", "source", "receivedAt"} {
		if _, ok := m[key]; !ok {
			t.Errorf("marshaled task missing key %q; got %s", key, b)
		}
	}
	// digest is omitempty and unset here, so it must be absent.
	if _, ok := m["digest"]; ok {
		t.Errorf("digest must be omitted when empty; got %s", b)
	}
	if v, ok := m["schemaVersion"].(float64); !ok || int(v) != SchemaVersion {
		t.Errorf("schemaVersion = %v, want %d", m["schemaVersion"], SchemaVersion)
	}
}

func TestDeployTaskDigestMarshaledWhenSet(t *testing.T) {
	task := NewDeployTask("quay", "holos/sample-app", "v2", "", time.Now())
	task.Digest = "sha256:abc"
	b, err := json.Marshal(task)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(b), `"digest":"sha256:abc"`) {
		t.Errorf("digest not marshaled when set; got %s", b)
	}
}

func TestIdempotencyKeyStability(t *testing.T) {
	// The same source event must yield a byte-identical key regardless of
	// when (or how many times) it is computed.
	k1 := IdempotencyKey("quay", "holos/sample-app", "v2", "quay.example.com/holos/sample-app")
	k2 := IdempotencyKey("quay", "holos/sample-app", "v2", "quay.example.com/holos/sample-app")
	if k1 != k2 {
		t.Errorf("idempotency key not stable: %q != %q", k1, k2)
	}

	// A task built from the same event carries the same key.
	t1 := NewDeployTask("quay", "holos/sample-app", "v2", "quay.example.com/holos/sample-app", time.Now())
	t2 := NewDeployTask("quay", "holos/sample-app", "v2", "quay.example.com/holos/sample-app", time.Now().Add(time.Hour))
	if t1.IdempotencyKey != t2.IdempotencyKey {
		t.Errorf("key depends on time: %q != %q", t1.IdempotencyKey, t2.IdempotencyKey)
	}
}

func TestIdempotencyKeyDistinct(t *testing.T) {
	base := IdempotencyKey("quay", "holos/sample-app", "v2", "url")
	cases := map[string]string{
		"different tag":       IdempotencyKey("quay", "holos/sample-app", "v3", "url"),
		"different repo":      IdempotencyKey("quay", "holos/other-app", "v2", "url"),
		"different source":    IdempotencyKey("github", "holos/sample-app", "v2", "url"),
		"different dockerURL": IdempotencyKey("quay", "holos/sample-app", "v2", "other"),
	}
	for name, k := range cases {
		if k == base {
			t.Errorf("%s: key collided with base %q", name, base)
		}
	}
}

func TestAppFromRepository(t *testing.T) {
	cases := map[string]string{
		"holos/sample-app": "sample-app",
		"sample-app":       "sample-app",
		"a/b/c":            "c",
		"":                 "",
	}
	for in, want := range cases {
		if got := AppFromRepository(in); got != want {
			t.Errorf("AppFromRepository(%q) = %q, want %q", in, got, want)
		}
	}
}
