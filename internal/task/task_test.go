package task

import (
	"testing"
	"time"

	pipelinev1alpha1 "github.com/holos-run/holos-paas/internal/gen/holos/paas/pipeline/v1alpha1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

func TestNewDeployTaskFields(t *testing.T) {
	receivedAt := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
	got := NewDeployTask("quay", "holos/sample-app", "v2", "quay.example.com/holos/sample-app", receivedAt)

	if got.GetSchemaVersion() != SchemaVersion {
		t.Errorf("SchemaVersion = %d, want %d", got.GetSchemaVersion(), SchemaVersion)
	}
	if got.GetApplication().GetName() != "sample-app" {
		t.Errorf("Application.Name = %q, want %q", got.GetApplication().GetName(), "sample-app")
	}
	if got.GetRepository() != "holos/sample-app" {
		t.Errorf("Repository = %q, want %q", got.GetRepository(), "holos/sample-app")
	}
	if got.GetTag() != "v2" {
		t.Errorf("Tag = %q, want %q", got.GetTag(), "v2")
	}
	if got.GetSource() != "quay" {
		t.Errorf("Source = %q, want %q", got.GetSource(), "quay")
	}
	if got.GetDigest() != "" {
		t.Errorf("Digest = %q, want empty", got.GetDigest())
	}
	if !got.GetReceivedAt().AsTime().Equal(receivedAt) {
		t.Errorf("ReceivedAt = %v, want %v", got.GetReceivedAt().AsTime(), receivedAt)
	}
	if got.GetIdempotencyKey() == "" {
		t.Error("IdempotencyKey is empty")
	}
}

// TestDeployTaskProtoRoundTrip is the ADR-14 wire-format test: a task marshaled
// to binary protobuf decodes back to an equal message. This is the encoding the
// subscriber publishes on tasks.deploy and the deployer consumes.
func TestDeployTaskProtoRoundTrip(t *testing.T) {
	receivedAt := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
	want := NewDeployTask("quay", "holos/sample-app", "v2", "", receivedAt)

	b, err := proto.Marshal(want)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var got pipelinev1alpha1.DeployTask
	if err := proto.Unmarshal(b, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !proto.Equal(want, &got) {
		t.Errorf("round-trip mismatch:\n got = %v\nwant = %v", &got, want)
	}
}

// TestDeployTaskFieldNumbers pins the wire-contract field numbers that form the
// binary protobuf layout (ADR-14). The serialized bytes carry only field
// numbers, so a renumbering is a wire break this test catches even when the Go
// field names are unchanged.
func TestDeployTaskFieldNumbers(t *testing.T) {
	fields := pipelinev1alpha1.File_holos_paas_pipeline_v1alpha1_pipeline_proto.
		Messages().ByName("DeployTask").Fields()
	want := map[string]int32{
		"schema_version":  1,
		"idempotency_key": 2,
		"application":     3,
		"repository":      4,
		"tag":             5,
		"digest":          6,
		"source":          7,
		"received_at":     8,
	}
	if got := fields.Len(); got != len(want) {
		t.Errorf("DeployTask has %d fields, want %d", got, len(want))
	}
	for name, num := range want {
		f := fields.ByName(protoreflect.Name(name))
		if f == nil {
			t.Errorf("DeployTask missing field %q", name)
			continue
		}
		if int32(f.Number()) != num {
			t.Errorf("field %q number = %d, want %d", name, f.Number(), num)
		}
	}
}

func TestDeployTaskDigestMarshaledWhenSet(t *testing.T) {
	task := NewDeployTask("quay", "holos/sample-app", "v2", "", time.Now())
	task.Digest = "sha256:abc"
	b, err := proto.Marshal(task)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got pipelinev1alpha1.DeployTask
	if err := proto.Unmarshal(b, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.GetDigest() != "sha256:abc" {
		t.Errorf("digest = %q, want %q", got.GetDigest(), "sha256:abc")
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
	if t1.GetIdempotencyKey() != t2.GetIdempotencyKey() {
		t.Errorf("key depends on time: %q != %q", t1.GetIdempotencyKey(), t2.GetIdempotencyKey())
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

// TestIdempotencyKeyNoFieldBoundaryCollision verifies the length-prefixed
// framing is injective: shifting a byte (here a NUL) across a field boundary
// produces a distinct key, so no two distinct field tuples collide.
func TestIdempotencyKeyNoFieldBoundaryCollision(t *testing.T) {
	a := IdempotencyKey("quay", "holos/app", "v1", "\x00x")
	b := IdempotencyKey("quay", "holos/app", "v1\x00", "x")
	if a == b {
		t.Errorf("field-boundary collision: %q == %q", a, b)
	}

	// Concatenation collisions must also be impossible across other fields.
	c := IdempotencyKey("quay", "holos/app", "", "v1")
	d := IdempotencyKey("quay", "holos/app", "v1", "")
	if c == d {
		t.Errorf("empty-field collision: %q == %q", c, d)
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
