package v1alpha1

import (
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// TestAddToSchemeRegistersKinds verifies that AddToScheme registers both the
// Organization and Repository kinds (and their List types) under the
// quay.holos.run/v1alpha1 group-version. This is the scaffold's smoke test: it
// catches an unregistered type before any reconciler depends on the scheme.
func TestAddToSchemeRegistersKinds(t *testing.T) {
	s := runtime.NewScheme()
	if err := AddToScheme(s); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}

	for _, kind := range []string{"Organization", "OrganizationList", "Repository", "RepositoryList"} {
		gvk := schema.GroupVersionKind{Group: "quay.holos.run", Version: "v1alpha1", Kind: kind}
		if !s.Recognizes(gvk) {
			t.Errorf("scheme does not recognize %s", gvk)
		}
	}
}

// TestGroupVersion pins the group and version so an accidental rename is caught.
func TestGroupVersion(t *testing.T) {
	if GroupVersion.Group != "quay.holos.run" {
		t.Errorf("group = %q, want quay.holos.run", GroupVersion.Group)
	}
	if GroupVersion.Version != "v1alpha1" {
		t.Errorf("version = %q, want v1alpha1", GroupVersion.Version)
	}
}

// TestDeepCopyRoundTrip exercises the generated DeepCopy methods on a populated
// Organization and Repository — a non-trivial smoke test that the generated
// code copies nested pointer fields (the Repository webhook) independently.
func TestDeepCopyRoundTrip(t *testing.T) {
	url := "https://kargo.holos.localhost/webhook/quay/abc"
	repo := &Repository{
		Spec: RepositorySpec{
			OrganizationRef: "my-project",
			Name:            "my-project-config",
			Visibility:      RepositoryVisibilityPrivate,
			Webhook:         &RepositoryWebhook{Url: &url},
		},
	}
	clone := repo.DeepCopy()
	if clone.Spec.Webhook == repo.Spec.Webhook {
		t.Error("DeepCopy did not clone the Webhook pointer")
	}
	if clone.Spec.Webhook.Url == repo.Spec.Webhook.Url {
		t.Error("DeepCopy did not clone the Webhook.Url pointer")
	}
	if *clone.Spec.Webhook.Url != url {
		t.Errorf("cloned Webhook.Url = %q, want %q", *clone.Spec.Webhook.Url, url)
	}

	perm := RepositoryRoleWrite
	org := &Organization{Spec: OrganizationSpec{
		Name:  "my-project",
		Email: "a@b.c",
		SyncedTeams: []SyncedTeam{{
			Name:                 "developers",
			OIDCGroup:            "my-project-developers",
			Role:                 OrganizationTeamRoleMember,
			RepositoryPermission: &perm,
		}},
	}}
	orgClone := org.DeepCopy()
	if orgClone.Spec.Name != "my-project" {
		t.Error("Organization DeepCopy lost Spec.Name")
	}
	if &orgClone.Spec.SyncedTeams[0] == &org.Spec.SyncedTeams[0] {
		t.Error("DeepCopy did not clone the SyncedTeams slice")
	}
	if orgClone.Spec.SyncedTeams[0].RepositoryPermission == org.Spec.SyncedTeams[0].RepositoryPermission {
		t.Error("DeepCopy did not clone the SyncedTeam.RepositoryPermission pointer")
	}
	if *orgClone.Spec.SyncedTeams[0].RepositoryPermission != RepositoryRoleWrite {
		t.Errorf("cloned RepositoryPermission = %q, want write", *orgClone.Spec.SyncedTeams[0].RepositoryPermission)
	}
}

// TestDefaultCredentialsSecretName pins the documented default Secret name so the
// API doc comment, the kubebuilder default marker, and consumers stay in sync.
func TestDefaultCredentialsSecretName(t *testing.T) {
	if DefaultCredentialsSecretName != "holos-controller-quay-creds" {
		t.Errorf("DefaultCredentialsSecretName = %q, want holos-controller-quay-creds", DefaultCredentialsSecretName)
	}
}
