package dataaccess

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// booth-lakehouse's GET /api/warehouse through core's gateway, as the app: its own token and its
// workspace. A stand-in answers per workspace, as booth-lakehouse does.
func TestLakehouse_Lookup(t *testing.T) {
	r := newRig(t)
	var gotAuth, gotWS, gotPath string
	answers := map[string]func(w http.ResponseWriter){
		"acme": func(w http.ResponseWriter) {
			_ = json.NewEncoder(w).Encode(map[string]any{"workspace": "acme", "backendId": "lake", "path": "acme-data",
				"warehouseName": "booth-ws-acme", "storageRoot": "s3://lake/acme-data", "createdBy": "o", "createdAt": 1})
		},
		"none":   func(w http.ResponseWriter) { http.Error(w, `{"detail":"no warehouse yet"}`, http.StatusNotFound) },
		"broken": func(w http.ResponseWriter) { http.Error(w, "boom", http.StatusBadGateway) },
		"weird": func(w http.ResponseWriter) {
			_ = json.NewEncoder(w).Encode(map[string]any{"backendId": "lake", "path": "p", "storageRoot": "file:///etc"})
		},
		"badid": func(w http.ResponseWriter) {
			_ = json.NewEncoder(w).Encode(map[string]any{"backendId": "../x", "path": "p", "storageRoot": "s3://b/p"})
		},
	}
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		gotAuth, gotWS, gotPath = req.Header.Get("Authorization"), req.Header.Get("X-Workspace"), req.URL.Path
		answers[gotWS](w)
	}))
	defer gw.Close()
	lh := &Lakehouse{GatewayURL: gw.URL + "/", Tokens: r.tok}
	ctx := context.Background()

	a := r.app(t, "acme", "o")
	wh, err := lh.Lookup(ctx, a)
	if err != nil || wh == nil {
		t.Fatalf("Lookup: %v %v", wh, err)
	}
	if *wh != (Warehouse{BackendID: "lake", Path: "acme-data", StorageRoot: "s3://lake/acme-data"}) {
		t.Errorf("warehouse %+v", *wh)
	}
	if gotPath != "/modules/lakehouse/api/warehouse" || gotWS != "acme" || gotAuth != "Bearer "+fakeJWT(Subject("acme", a.ID)) {
		t.Errorf("request: path %q workspace %q auth %q; want the app's own token and workspace", gotPath, gotWS, gotAuth)
	}

	if wh, err := lh.Lookup(ctx, r.app(t, "none", "o")); wh != nil || err != nil {
		t.Errorf("404 must mean no warehouse and no error, got %v %v", wh, err)
	}
	for _, ws := range []string{"broken", "weird", "badid"} {
		if wh, err := lh.Lookup(ctx, r.app(t, ws, "o")); wh != nil || err == nil {
			t.Errorf("%s: want an error and no warehouse, got %v %v", ws, wh, err)
		}
	}

	// A paused owner: no token, so no lookup at all.
	r.m.refuse["gone"] = true
	gotWS = ""
	if wh, err := lh.Lookup(ctx, r.app(t, "acme", "gone")); wh != nil || err == nil || !strings.Contains(err.Error(), "no longer has access") {
		t.Errorf("paused owner: %v %v", wh, err)
	}
	if gotWS != "" {
		t.Error("looked up a warehouse without a token")
	}
}
