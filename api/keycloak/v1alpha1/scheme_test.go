package v1alpha1

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// TestAddToSchemeRegistersKinds verifies that AddToScheme registers every Kind
// (and its List type) under the keycloak.holos.run/v1alpha1 group-version. This
// is the scaffold's smoke test: it catches an unregistered type before any
// reconciler depends on the scheme.
func TestAddToSchemeRegistersKinds(t *testing.T) {
	s := runtime.NewScheme()
	if err := AddToScheme(s); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}

	for _, kind := range []string{
		"KeycloakInstance", "KeycloakInstanceList",
		"KeycloakGroup", "KeycloakGroupList",
		"KeycloakGroupMembership", "KeycloakGroupMembershipList",
		"KeycloakUser", "KeycloakUserList",
		"KeycloakClient", "KeycloakClientList",
	} {
		gvk := schema.GroupVersionKind{Group: "keycloak.holos.run", Version: "v1alpha1", Kind: kind}
		if !s.Recognizes(gvk) {
			t.Errorf("scheme does not recognize %s", gvk)
		}
	}
}

// TestGroupVersion pins the group and version so an accidental rename is caught.
func TestGroupVersion(t *testing.T) {
	if GroupVersion.Group != "keycloak.holos.run" {
		t.Errorf("group = %q, want keycloak.holos.run", GroupVersion.Group)
	}
	if GroupVersion.Version != "v1alpha1" {
		t.Errorf("version = %q, want v1alpha1", GroupVersion.Version)
	}
}

// TestDeepCopyRoundTrip exercises the generated DeepCopy methods on populated
// resources — a non-trivial smoke test that the generated code copies nested
// pointer and slice fields independently.
func TestDeepCopyRoundTrip(t *testing.T) {
	client := &KeycloakClient{
		// ClientRef is the KeycloakClient's metadata.name (an object name), not the
		// URL-shaped clientId — the reconciler derives the clientId from the
		// referenced CR's spec.clientId (see ClientRoleReference).
		Spec: KeycloakClientSpec{
			ClientID:     "https://my-app.holos.internal",
			Type:         KeycloakClientTypeConfidential,
			InstanceRef:  KeycloakInstanceReference{Name: "holos", Namespace: "holos-controller"},
			RedirectURIs: []string{"https://my-app.holos.internal/oauth2/callback"},
			ClientRoles:  []ClientRoleReference{{ClientRef: "my-app", Role: "editor"}},
			SecretRef:    &ClientSecretReference{Name: "my-app-oidc", Key: "client_secret"},
			CABundle:     []byte("-----BEGIN CERTIFICATE-----"),
		},
	}
	clone := client.DeepCopy()
	if clone.Spec.SecretRef == client.Spec.SecretRef {
		t.Error("DeepCopy did not clone the SecretRef pointer")
	}
	if &clone.Spec.ClientRoles[0] == &client.Spec.ClientRoles[0] {
		t.Error("DeepCopy did not clone the ClientRoles slice")
	}
	if &clone.Spec.CABundle[0] == &client.Spec.CABundle[0] {
		t.Error("DeepCopy did not clone the CABundle slice")
	}

	group := &KeycloakGroup{
		Spec: KeycloakGroupSpec{
			Path:        "projects/my-project/roles/owner",
			InstanceRef: KeycloakInstanceReference{Name: "holos"},
			ClientRoles: []ClientRoleReference{{ClientRef: "my-app", Role: "owner"}},
			Custodians:  []CustodianReference{{Path: "projects/my-project/custodians/owner"}},
		},
	}
	groupClone := group.DeepCopy()
	if &groupClone.Spec.Custodians[0] == &group.Spec.Custodians[0] {
		t.Error("DeepCopy did not clone the Custodians slice")
	}

	lastValidated := metav1.Now()
	lastMutated := metav1.Now()
	lastDrift := metav1.Now()
	membership := &KeycloakGroupMembership{
		Spec: KeycloakGroupMembershipSpec{
			InstanceRef: KeycloakInstanceReference{Name: "holos"},
			GroupRef:    KeycloakGroupReference{Name: "my-project-roles-owner"},
			Members:     []KeycloakGroupMembershipMember{{Email: "bob@example.com"}},
		},
		Status: KeycloakGroupMembershipStatus{
			GroupID:            "group-uuid",
			ManagedMembers:     []ManagedKeycloakGroupMember{{Email: "bob@example.com", UserID: "user-uuid"}},
			LastValidatedTime:  &lastValidated,
			LastMutatedTime:    &lastMutated,
			LastMutationReason: MutationReasonDriftRemediation,
			LastDriftTime:      &lastDrift,
		},
	}
	membershipClone := membership.DeepCopy()
	if &membershipClone.Spec.Members[0] == &membership.Spec.Members[0] {
		t.Error("DeepCopy did not clone the Members slice")
	}
	if &membershipClone.Status.ManagedMembers[0] == &membership.Status.ManagedMembers[0] {
		t.Error("DeepCopy did not clone the ManagedMembers slice")
	}
	if membershipClone.Status.LastValidatedTime == membership.Status.LastValidatedTime {
		t.Error("DeepCopy did not clone the LastValidatedTime pointer")
	}
	if membershipClone.Status.LastMutatedTime == membership.Status.LastMutatedTime {
		t.Error("DeepCopy did not clone the LastMutatedTime pointer")
	}
	if membershipClone.Status.LastDriftTime == membership.Status.LastDriftTime {
		t.Error("DeepCopy did not clone the LastDriftTime pointer")
	}
	if *membershipClone.Status.LastValidatedTime != *membership.Status.LastValidatedTime {
		t.Error("DeepCopy changed LastValidatedTime")
	}

	user := &KeycloakUser{
		Spec: KeycloakUserSpec{
			Email:                "bob@example.com",
			InstanceRef:          KeycloakInstanceReference{Name: "holos"},
			Groups:               []string{"projects/my-project/roles/editor"},
			IdentityProviderLink: &IdentityProviderLink{Alias: "google"},
		},
	}
	userClone := user.DeepCopy()
	if userClone.Spec.IdentityProviderLink == user.Spec.IdentityProviderLink {
		t.Error("DeepCopy did not clone the IdentityProviderLink pointer")
	}
	if &userClone.Spec.Groups[0] == &user.Spec.Groups[0] {
		t.Error("DeepCopy did not clone the Groups slice")
	}
}

// TestDefaultCredentialsSecretName pins the documented default Secret name so the
// API doc comment, the kubebuilder default marker, and consumers stay in sync.
func TestDefaultCredentialsSecretName(t *testing.T) {
	if DefaultCredentialsSecretName != "holos-controller-keycloak-creds" {
		t.Errorf("DefaultCredentialsSecretName = %q, want holos-controller-keycloak-creds", DefaultCredentialsSecretName)
	}
}
