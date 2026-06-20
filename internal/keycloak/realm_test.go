package keycloak

import (
	"context"
	"net/http"
	"testing"
)

const realmBase = adminPathPrefix + "/realms/holos"

func TestGetRealm(t *testing.T) {
	h := &recordingHandler{
		t: t, wantMethod: http.MethodGet, wantPath: realmBase,
		status:   http.StatusOK,
		respBody: `{"id":"holos","realm":"holos","enabled":true}`,
	}
	c, _ := newTestClient(t, h)

	realm, err := c.GetRealm(context.Background())
	if err != nil {
		t.Fatalf("GetRealm: %v", err)
	}
	assertCommonRequest(t, h, false)
	if realm.Realm != "holos" || !realm.Enabled {
		t.Errorf("decoded realm = %+v", realm)
	}
}

func TestGetRealmNotFound(t *testing.T) {
	h := &recordingHandler{
		t: t, wantMethod: http.MethodGet, wantPath: realmBase,
		status:   http.StatusNotFound,
		respBody: `{"error":"Realm not found."}`,
	}
	c, _ := newTestClient(t, h)

	if _, err := c.GetRealm(context.Background()); !IsNotFound(err) {
		t.Fatalf("expected IsNotFound, got %v", err)
	}
}
