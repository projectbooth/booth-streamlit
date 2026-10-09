package dataaccess

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/projectbooth/booth-streamlit/internal/identity"
)

// The editor's dataset list is read as the signed-in owner: a token minted for them (not for an
// app), sent to booth-catalog through core's gateway with their workspace. Anything more than
// viewer is refused, as for apps.
func TestCatalogDatasets_ListsAsTheCaller(t *testing.T) {
	r := newRig(t)
	var gotAuth, gotWS, gotPath, gotQ string
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		gotAuth, gotWS, gotPath, gotQ = req.Header.Get("Authorization"), req.Header.Get("X-Workspace"), req.URL.Path, req.URL.RawQuery
		_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{{"id": "ds-1", "name": "Orders", "format": "file", "location": map[string]any{}}}, "total": 1})
	}))
	defer gw.Close()
	c := &CatalogDatasets{GatewayURL: gw.URL, Minter: r.m}
	alice := identity.Caller{Subject: "alice", Workspace: "acme", Role: identity.RoleOwner}

	items, total, err := c.List(context.Background(), alice, "ord ers")
	if err != nil || total != 1 || len(items) != 1 || items[0] != (DatasetSummary{ID: "ds-1", Name: "Orders", Format: "file"}) {
		t.Fatalf("List: %v %d %v", items, total, err)
	}
	if want := "acme|streamlit:acme:sources|alice"; len(r.m.calls) != 1 || r.m.calls[0] != want {
		t.Errorf("mints %v, want [%s]", r.m.calls, want)
	}
	if gotPath != "/modules/catalog/api/datasets" || gotQ != "limit=200&q=ord+ers" || gotWS != "acme" || gotAuth != "Bearer "+fakeJWT("streamlit:acme:sources") {
		t.Errorf("request %s?%s workspace %s auth %s", gotPath, gotQ, gotWS, gotAuth)
	}

	r.m.role = "editor"
	if _, _, err := c.List(context.Background(), alice, ""); !errors.Is(err, ErrRoleExceeded) {
		t.Errorf("more than viewer: %v", err)
	}
	r.m.role = ""
	r.m.refuse["alice"] = true
	if _, _, err := c.List(context.Background(), alice, ""); !errors.Is(err, ErrOwnerNoAccess) {
		t.Errorf("refused mint: %v", err)
	}
}
