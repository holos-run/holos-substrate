package v1alpha1

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"unicode"

	"k8s.io/apimachinery/pkg/runtime"
)

const keycloakAPITypePackage = "github.com/holos-run/holos-substrate/api/keycloak/v1alpha1"

// keycloakAdminEventResourceTypes vendors org.keycloak.events.admin.ResourceType
// from Keycloak 26.6.3:
// https://github.com/keycloak/keycloak/blob/26.6.3/server-spi-private/src/main/java/org/keycloak/events/admin/ResourceType.java
var keycloakAdminEventResourceTypes = map[string]struct{}{
	"REALM":                         {},
	"REALM_ROLE":                    {},
	"REALM_ROLE_MAPPING":            {},
	"REALM_SCOPE_MAPPING":           {},
	"AUTH_FLOW":                     {},
	"AUTH_EXECUTION_FLOW":           {},
	"AUTH_EXECUTION":                {},
	"AUTHENTICATOR_CONFIG":          {},
	"REQUIRED_ACTION_CONFIG":        {},
	"REQUIRED_ACTION":               {},
	"IDENTITY_PROVIDER":             {},
	"IDENTITY_PROVIDER_MAPPER":      {},
	"PROTOCOL_MAPPER":               {},
	"USER":                          {},
	"USER_LOGIN_FAILURE":            {},
	"USER_SESSION":                  {},
	"USER_FEDERATION_PROVIDER":      {},
	"USER_FEDERATION_MAPPER":        {},
	"GROUP":                         {},
	"GROUP_MEMBERSHIP":              {},
	"CLIENT":                        {},
	"CLIENT_INITIAL_ACCESS_MODEL":   {},
	"CLIENT_ROLE":                   {},
	"CLIENT_ROLE_MAPPING":           {},
	"CLIENT_SCOPE":                  {},
	"CLIENT_SCOPE_MAPPING":          {},
	"CLIENT_SCOPE_CLIENT_MAPPING":   {},
	"CLUSTER_NODE":                  {},
	"COMPONENT":                     {},
	"AUTHORIZATION_RESOURCE_SERVER": {},
	"AUTHORIZATION_RESOURCE":        {},
	"AUTHORIZATION_SCOPE":           {},
	"AUTHORIZATION_POLICY":          {},
	"CUSTOM":                        {},
	"USER_PROFILE":                  {},
	"ORGANIZATION":                  {},
	"ORGANIZATION_MEMBERSHIP":       {},
	"ORGANIZATION_GROUP":            {},
	"ORGANIZATION_GROUP_MEMBERSHIP": {},
}

// kindMappingExceptions contains Kinds that intentionally front no Keycloak
// resource represented by an admin-event ResourceType. Every entry must name a
// registered Kind and carry a justification so exceptions remain explicit.
var kindMappingExceptions = map[string]string{
	"Instance": "connection/validator resource for a Keycloak server and realm; no upstream admin-event resource type",
}

func TestKeycloakKindsMapToAdminEventResourceTypes(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}

	var kinds []string
	registeredKinds := make(map[string]struct{})
	for gvk, objectType := range scheme.AllKnownTypes() {
		if gvk.GroupVersion() != GroupVersion || strings.HasSuffix(gvk.Kind, "List") {
			continue
		}
		for objectType.Kind() == reflect.Ptr {
			objectType = objectType.Elem()
		}
		if objectType.PkgPath() != keycloakAPITypePackage {
			continue
		}
		kinds = append(kinds, gvk.Kind)
		registeredKinds[gvk.Kind] = struct{}{}
	}
	if len(kinds) == 0 {
		t.Fatal("no keycloak.holos.run API Kinds found in the package scheme")
	}

	sort.Strings(kinds)
	for _, kind := range kinds {
		t.Run(kind, func(t *testing.T) {
			if err := validateKindResourceType(kind); err != nil {
				t.Error(err)
			}
		})
	}

	for kind, justification := range kindMappingExceptions {
		if strings.TrimSpace(justification) == "" {
			t.Errorf("kindMappingExceptions[%q] has empty justification %q", kind, justification)
		}
		if _, ok := registeredKinds[kind]; !ok {
			t.Errorf("kindMappingExceptions[%q] with justification %q does not name a registered, non-List keycloak.holos.run Kind", kind, justification)
		}
	}
}

func TestCamelCaseToScreamingSnake(t *testing.T) {
	tests := map[string]string{
		"":                "",
		"Group":           "GROUP",
		"GroupMembership": "GROUP_MEMBERSHIP",
		"OIDCThing":       "OIDC_THING",
		"HTTPServerURL":   "HTTP_SERVER_URL",
		"Client2Role":     "CLIENT2_ROLE",
	}

	for input, want := range tests {
		t.Run(input, func(t *testing.T) {
			if got := camelCaseToScreamingSnake(input); got != want {
				t.Errorf("camelCaseToScreamingSnake(%q) = %q, want %q", input, got, want)
			}
		})
	}
}

func TestValidateKindResourceType(t *testing.T) {
	tests := []struct {
		name    string
		kind    string
		wantErr []string
	}{
		{name: "mapped", kind: "GroupMembership"},
		{name: "allowlisted", kind: "Instance"},
		{
			name: "unmapped",
			kind: "ProjectSpace",
			wantErr: []string{
				`Kind "ProjectSpace"`,
				`"PROJECT_SPACE"`,
				"CamelCase to SCREAMING_SNAKE",
				"add an explicit kindMappingExceptions entry with justification",
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateKindResourceType(test.kind)
			if len(test.wantErr) == 0 {
				if err != nil {
					t.Fatalf("validateKindResourceType(%q): %v", test.kind, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("validateKindResourceType(%q) returned nil, want an actionable error", test.kind)
			}
			for _, part := range test.wantErr {
				if !strings.Contains(err.Error(), part) {
					t.Errorf("error %q does not contain %q", err, part)
				}
			}
		})
	}
}

func validateKindResourceType(kind string) error {
	if _, ok := kindMappingExceptions[kind]; ok {
		return nil
	}

	resourceType := camelCaseToScreamingSnake(kind)
	if _, ok := keycloakAdminEventResourceTypes[resourceType]; ok {
		return nil
	}

	return fmt.Errorf(
		"keycloak.holos.run Kind %q derives admin-event resource type %q by the CamelCase to SCREAMING_SNAKE rule, but Keycloak 26.6.3 org.keycloak.events.admin.ResourceType has no such value; name the Kind to match an upstream resource type, or add an explicit kindMappingExceptions entry with justification when the Kind fronts no upstream resource",
		kind,
		resourceType,
	)
}

func camelCaseToScreamingSnake(value string) string {
	runes := []rune(value)
	var result strings.Builder
	for i, current := range runes {
		if i > 0 && unicode.IsUpper(current) {
			previous := runes[i-1]
			nextIsLower := i+1 < len(runes) && unicode.IsLower(runes[i+1])
			if unicode.IsLower(previous) || unicode.IsDigit(previous) || (unicode.IsUpper(previous) && nextIsLower) {
				result.WriteByte('_')
			}
		}
		result.WriteRune(unicode.ToUpper(current))
	}
	return result.String()
}
