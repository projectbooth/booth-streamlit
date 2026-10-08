package api

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/projectbooth/booth-streamlit/internal/apps"
	"github.com/projectbooth/booth-streamlit/internal/identity"
)

const testIssuer = "http://booth-core.booth-system.svc.cluster.local:8080/iframe-identity"

// harness is the real router and the real identity verifier, with assertions signed by a test key
// in exactly the shape booth-core's iframeidentity.Service mints them. Roles therefore come from
// the verified assertion, as in production (ADR 0041), never from a fake.
type harness struct {
	t     *testing.T
	jws   jose.Signer
	h     http.Handler
	store *apps.MemoryStore
	svc   *apps.Service
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	jws, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, (&jose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		t.Fatal(err)
	}
	v := identity.NewWithKeySet(identity.Config{IssuerURL: testIssuer}, &oidc.StaticKeySet{PublicKeys: []crypto.PublicKey{&key.PublicKey}})
	store := apps.NewMemoryStore()
	svc := apps.NewService(store, 0)
	return &harness{t: t, jws: jws, store: store, svc: svc, h: NewRouter(Deps{DB: fakeDB{}, Verifier: v, Apps: svc})}
}

// as returns headers for a person with role in workspace, as core's iframe proxy sends them.
func (h *harness) as(sub, workspace string, role identity.Role) http.Header {
	h.t.Helper()
	now := time.Now()
	raw, err := jwt.Signed(h.jws).Claims(jwt.Claims{
		Issuer: testIssuer, Subject: sub, Audience: jwt.Audience{identity.ModuleID},
		IssuedAt: jwt.NewNumericDate(now), NotBefore: jwt.NewNumericDate(now), Expiry: jwt.NewNumericDate(now.Add(2 * time.Minute)),
	}).Claims(map[string]any{"groups": []string{"/workspaces/" + workspace + "/" + string(role)}, "booth_module": identity.ModuleID}).Serialize()
	if err != nil {
		h.t.Fatal(err)
	}
	return http.Header{
		identity.HeaderIdentity:  {raw},
		identity.HeaderWorkspace: {workspace},
		identity.HeaderRole:      {string(role)},
	}
}

func (h *harness) do(method, path string, hdr http.Header, body any) (int, map[string]any) {
	h.t.Helper()
	var rd *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rd)
	for k, v := range hdr {
		req.Header[k] = v
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.h.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

var (
	ownerSub  = "owner-1"
	editorSub = "editor-1"
	viewerSub = "viewer-1"
	otherSub  = "other-owner-1"
)

// seed creates one shared and one unshared app in acme, as an owner, through the service.
func (h *harness) seed() (shared, private apps.App) {
	h.t.Helper()
	owner := identity.Caller{Subject: ownerSub, Workspace: "acme", Role: identity.RoleOwner}
	var err error
	if shared, err = h.svc.Create(context.Background(), owner, apps.Input{Name: "Shared", Source: "import streamlit as st", Shared: true}); err != nil {
		h.t.Fatal(err)
	}
	if private, err = h.svc.Create(context.Background(), owner, apps.Input{Name: "Private", Source: "import streamlit as st"}); err != nil {
		h.t.Fatal(err)
	}
	return shared, private
}

type writePath struct {
	name, method, path string
	body               any
}

func writePaths(id string) []writePath {
	edit := apps.Input{Name: "Hijacked", Source: "print('owned')", Shared: true}
	return []writePath{
		{"edit", http.MethodPut, "/api/apps/" + id, edit},
		{"start", http.MethodPost, "/api/apps/" + id + "/start", nil},
		{"stop", http.MethodPost, "/api/apps/" + id + "/stop", nil},
		{"delete", http.MethodDelete, "/api/apps/" + id, nil},
	}
}

// ADR 0105: only workspace owners may create, edit, start, stop or delete apps. Editors and
// viewers of the app's workspace, and owners of another workspace, are refused on every write path,
// and the app is unchanged afterwards. The roles come from signed assertions through the real
// verifier.
func TestAppAPI_OnlyWorkspaceOwnersWrite(t *testing.T) {
	h := newHarness(t)
	shared, private := h.seed()

	refused := []struct {
		who     string
		hdr     http.Header
		visible int // status for a write to an app this caller can see (shared): 403
		hidden  int // status for one they can't (unshared, or another workspace): 404
	}{
		{"editor of acme", h.as(editorSub, "acme", identity.RoleEditor), 403, 404},
		{"viewer of acme", h.as(viewerSub, "acme", identity.RoleViewer), 403, 404},
		// Owner of a different workspace: nothing in acme exists for them.
		{"owner of other-team", h.as(otherSub, "other-team", identity.RoleOwner), 404, 404},
	}
	for _, c := range refused {
		t.Run(c.who, func(t *testing.T) {
			if code, _ := h.do(http.MethodPost, "/api/apps", c.hdr, apps.Input{Name: "New", Source: "x"}); c.who != "owner of other-team" && code != http.StatusForbidden {
				t.Errorf("create: %d, want 403", code)
			}
			for _, app := range []struct {
				a    apps.App
				want int
			}{{shared, c.visible}, {private, c.hidden}} {
				for _, p := range writePaths(app.a.ID) {
					code, body := h.do(p.method, p.path, c.hdr, p.body)
					if code != app.want {
						t.Errorf("%s %s app: %d %v, want %d", p.name, app.a.Name, code, body, app.want)
					}
				}
				after, err := h.store.Get(context.Background(), "acme", app.a.ID)
				if err != nil {
					t.Fatalf("%s app was deleted by a refused caller: %v", app.a.Name, err)
				}
				if after != app.a {
					t.Errorf("%s app changed after refused writes:\n got %+v\nwant %+v", app.a.Name, after, app.a)
				}
			}
		})
	}

	// The other workspace's owner may create in *their own* workspace; that app lands there and
	// never in acme.
	code, body := h.do(http.MethodPost, "/api/apps", h.as(otherSub, "other-team", identity.RoleOwner), apps.Input{Name: "Theirs", Source: "x"})
	if code != http.StatusCreated || body["workspace"] != "other-team" {
		t.Fatalf("other-team owner creating in their own workspace: %d %v", code, body)
	}
	list, _ := h.store.List(context.Background(), "acme", false)
	if len(list) != 2 {
		t.Errorf("acme has %d apps after other-team's create, want 2", len(list))
	}
}

// ADR 0041: an editor whose request claims X-Booth-Role: owner is refused outright, before any
// handler runs; the role is the assertion's, not the header's.
func TestAppAPI_ForgedRoleHeaderIsRefused(t *testing.T) {
	h := newHarness(t)
	shared, _ := h.seed()
	hdr := h.as(editorSub, "acme", identity.RoleEditor)
	hdr.Set(identity.HeaderRole, "owner")
	for _, p := range append(writePaths(shared.ID), writePath{"create", http.MethodPost, "/api/apps", apps.Input{Name: "x", Source: "x"}}) {
		if code, _ := h.do(p.method, p.path, hdr, p.body); code != http.StatusForbidden {
			t.Errorf("%s with a forged owner header: %d, want 403", p.name, code)
		}
	}
	if code, _ := h.do(http.MethodGet, "/api/apps", http.Header{}, nil); code != http.StatusUnauthorized {
		t.Errorf("no assertion: %d, want 401", code)
	}
}

func TestAppAPI_OwnerLifecycle(t *testing.T) {
	h := newHarness(t)
	owner := h.as(ownerSub, "acme", identity.RoleOwner)

	code, me := h.do(http.MethodGet, "/api/me", owner, nil)
	if code != 200 || me["canAuthor"] != true || me["role"] != "owner" || me["workspace"] != "acme" {
		t.Fatalf("/api/me: %d %v", code, me)
	}

	code, created := h.do(http.MethodPost, "/api/apps", owner, apps.Input{Name: " Sales ", Description: "by region", Source: "import streamlit as st\nst.title('Sales')"})
	if code != http.StatusCreated || created["name"] != "Sales" || created["desiredState"] != "stopped" || created["createdBy"] != ownerSub || created["shared"] != false {
		t.Fatalf("create: %d %v", code, created)
	}
	id := created["id"].(string)

	if code, got := h.do(http.MethodPut, "/api/apps/"+id, owner, apps.Input{Name: "Sales v2", Source: "x = 1", Shared: true}); code != 200 || got["name"] != "Sales v2" || got["shared"] != true {
		t.Fatalf("edit: %d %v", code, got)
	}
	if code, got := h.do(http.MethodPost, "/api/apps/"+id+"/start", owner, nil); code != 200 || got["desiredState"] != "running" {
		t.Fatalf("start: %d %v", code, got)
	}
	if code, got := h.do(http.MethodPost, "/api/apps/"+id+"/stop", owner, nil); code != 200 || got["desiredState"] != "stopped" {
		t.Fatalf("stop: %d %v", code, got)
	}
	if code, got := h.do(http.MethodGet, "/api/apps/"+id, owner, nil); code != 200 || got["source"] != "x = 1" {
		t.Fatalf("get: %d %v", code, got)
	}
	if code, _ := h.do(http.MethodDelete, "/api/apps/"+id, owner, nil); code != http.StatusNoContent {
		t.Fatalf("delete: %d", code)
	}
	if code, _ := h.do(http.MethodGet, "/api/apps/"+id, owner, nil); code != 404 {
		t.Fatalf("get after delete: %d", code)
	}
}

// Editors and viewers see only shared apps of their own workspace; owners see all of their
// workspace's; nobody sees another workspace's.
func TestAppAPI_Visibility(t *testing.T) {
	h := newHarness(t)
	shared, private := h.seed()
	names := func(hdr http.Header) string {
		code, body := h.do(http.MethodGet, "/api/apps", hdr, nil)
		if code != 200 {
			t.Fatalf("list: %d %v", code, body)
		}
		var out []string
		for _, a := range body["apps"].([]any) {
			m := a.(map[string]any)
			if _, has := m["source"]; has {
				t.Error("list includes source")
			}
			out = append(out, m["name"].(string))
		}
		return strings.Join(out, ",")
	}
	for _, c := range []struct {
		who  string
		hdr  http.Header
		want string
	}{
		{"owner", h.as(ownerSub, "acme", identity.RoleOwner), "Private,Shared"},
		{"editor", h.as(editorSub, "acme", identity.RoleEditor), "Shared"},
		{"viewer", h.as(viewerSub, "acme", identity.RoleViewer), "Shared"},
		{"other workspace's owner", h.as(otherSub, "other-team", identity.RoleOwner), ""},
	} {
		if got := names(c.hdr); got != c.want {
			t.Errorf("%s sees %q, want %q", c.who, got, c.want)
		}
	}

	viewer := h.as(viewerSub, "acme", identity.RoleViewer)
	if code, _ := h.do(http.MethodGet, "/api/apps/"+shared.ID, viewer, nil); code != 200 {
		t.Errorf("viewer opening a shared app: %d", code)
	}
	if code, _ := h.do(http.MethodGet, "/api/apps/"+private.ID, viewer, nil); code != 404 {
		t.Errorf("viewer opening an unshared app: %d, want 404", code)
	}
}

func TestAppAPI_InputHandling(t *testing.T) {
	h := newHarness(t)
	owner := h.as(ownerSub, "acme", identity.RoleOwner)
	for _, c := range []struct {
		name string
		body any
		want int
	}{
		{"no name", apps.Input{Source: "x"}, 400},
		{"no source", apps.Input{Name: "n", Source: "  "}, 400},
		{"name too long", apps.Input{Name: strings.Repeat("n", 101), Source: "x"}, 400},
		{"source too large", apps.Input{Name: "n", Source: strings.Repeat("x", apps.DefaultMaxSourceBytes+1)}, 400},
		{"unknown field", map[string]any{"name": "n", "source": "x", "workspace": "other-team"}, 400},
	} {
		if code, body := h.do(http.MethodPost, "/api/apps", owner, c.body); code != c.want {
			t.Errorf("%s: %d %v, want %d", c.name, code, body, c.want)
		}
	}

	// A form post (the only thing a cross-site page can send with the viewer's cookie under
	// SameSite=Lax rules for top-level POSTs) is refused by content type.
	req := httptest.NewRequest(http.MethodPost, "/api/apps", strings.NewReader("name=n&source=x"))
	for k, v := range owner {
		req.Header[k] = v
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Errorf("form post: %d, want 415", rec.Code)
	}
}
