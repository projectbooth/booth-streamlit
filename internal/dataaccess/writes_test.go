package dataaccess

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/projectbooth/booth-streamlit/internal/apps"
	"github.com/projectbooth/booth-streamlit/internal/identity"
)

// fakeAPI stands in for booth-core's gateway in front of booth-api's app write route and grant
// management API. It answers with a canned status and body, and records what it was sent.
type fakeAPI struct {
	mu     sync.Mutex
	seen   []*http.Request
	bodies []string
	status int
	body   string
	hdr    map[string]string
	srv    *httptest.Server
}

func newFakeAPI(t *testing.T) *fakeAPI {
	t.Helper()
	f := &fakeAPI{status: 201, body: `{"data":{"id":"1","amount":"12.50"},"version":"v1"}`}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.seen = append(f.seen, r.Clone(context.Background()))
		f.bodies = append(f.bodies, string(b))
		status, body, hdr := f.status, f.body, f.hdr
		f.mu.Unlock()
		for k, v := range hdr {
			w.Header().Set(k, v)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAPI) n() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.seen) }

func (f *fakeAPI) last() (*http.Request, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.seen) == 0 {
		return nil, ""
	}
	return f.seen[len(f.seen)-1], f.bodies[len(f.bodies)-1]
}

type writesRig struct {
	*rig
	api *fakeAPI
	srv *httptest.Server
	a   apps.App // acme, declares household: insert, update
	b   apps.App // other-team, declares nothing
}

func newWritesRig(t *testing.T) *writesRig {
	t.Helper()
	r := newRig(t)
	api := newFakeAPI(t)
	in := &Internal{Apps: r.svc, Tokens: r.tok, Writes: &Writes{GatewayURL: api.srv.URL, MaxConcurrent: 2}}
	wr := &writesRig{rig: r, api: api}
	wr.a = r.app(t, "acme", "alice")
	var err error
	wr.a, err = r.svc.SetWriteTargets(context.Background(), identity.Caller{Subject: "alice", Workspace: "acme", Role: identity.RoleOwner}, wr.a.ID,
		apps.WriteTargetsInput{Targets: []apps.WriteTarget{{API: "household", Operations: []string{"insert", "update"}}}, Confirmed: true})
	if err != nil {
		t.Fatal(err)
	}
	wr.b = r.app(t, "other-team", "olga")
	wr.srv = httptest.NewServer(in.Router())
	t.Cleanup(wr.srv.Close)
	return wr
}

func (wr *writesRig) do(t *testing.T, method, rawPath, bearer, body string, hdr map[string]string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(method, wr.srv.URL+"/x", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	path, query, _ := strings.Cut(rawPath, "?")
	req.URL.Opaque = "//" + req.URL.Host + path // send the path exactly as written
	req.URL.RawQuery = query
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func errCode(t *testing.T, body string) string {
	t.Helper()
	var e struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &e); err != nil {
		t.Fatalf("not booth-api's error shape: %s", body)
	}
	return e.Error.Code
}

func TestWrites_ForwardsADeclaredInsertWithTheAppsOwnToken(t *testing.T) {
	wr := newWritesRig(t)
	wr.api.hdr = map[string]string{"ETag": `"v1"`, "Idempotent-Replayed": "true", "Location": "/modules/api/app/v1/household/rows/1", "Set-Cookie": "x=y"}
	resp, body := wr.do(t, "POST", "/writes/household/rows", wr.a.GateBearer, `{"data":{"amount":"12.50"}}`, map[string]string{
		"Idempotency-Key": "k-1", "X-Booth-App-User": "user-123", "X-Workspace": "other-team", "Cookie": "booth_iframe_session=stolen",
	})
	if resp.StatusCode != 201 || !strings.Contains(body, `"version":"v1"`) {
		t.Fatalf("insert: %d %s", resp.StatusCode, body)
	}
	if resp.Header.Get("ETag") != `"v1"` || resp.Header.Get("Idempotent-Replayed") != "true" || resp.Header.Get("Set-Cookie") != "" {
		t.Errorf("relayed headers: %v (ETag and Idempotent-Replayed pass, nothing else)", resp.Header)
	}
	up, upBody := wr.api.last()
	if up.Method != "POST" || up.URL.Path != "/modules/api/app/v1/household/rows" || upBody != `{"data":{"amount":"12.50"}}` {
		t.Errorf("upstream %s %s %q", up.Method, up.URL.Path, upBody)
	}
	// The app's existing token, unchanged: subject streamlit:<ws>:<appId>, the same one the gate and
	// the file proxy use (no second token is minted).
	jwt := strings.TrimPrefix(up.Header.Get("Authorization"), "Bearer ")
	if sub, _ := unverifiedSubject(jwt); sub != Subject("acme", wr.a.ID) {
		t.Errorf("forwarded a token for %q, want the app's", sub)
	}
	cached, _ := wr.tok.Get(context.Background(), wr.a)
	if jwt != cached.JWT {
		t.Error("forwarded a different token from the app's cached one")
	}
	// The workspace is the app's, never the caller's header; no cookie goes upstream.
	if up.Header.Get("X-Workspace") != "acme" || up.Header.Get("Cookie") != "" {
		t.Errorf("upstream X-Workspace=%q Cookie=%q", up.Header.Get("X-Workspace"), up.Header.Get("Cookie"))
	}
	if up.Header.Get("Idempotency-Key") != "k-1" || up.Header.Get("X-Booth-App-User") != "user-123" || up.Header.Get("Content-Type") != "application/json" {
		t.Errorf("forwarded headers: %v", up.Header)
	}
}

func TestWrites_UpdateAndKeyForms(t *testing.T) {
	wr := newWritesRig(t)
	wr.api.status, wr.api.body = 200, `{"data":{},"version":"v2"}`
	if resp, body := wr.do(t, "PATCH", "/writes/household/rows/a%20b", wr.a.GateBearer, `{"data":{"note":"x"}}`, map[string]string{"If-Match": `"v1"`}); resp.StatusCode != 200 {
		t.Fatalf("update: %d %s", resp.StatusCode, body)
	}
	up, _ := wr.api.last()
	if up.URL.EscapedPath() != "/modules/api/app/v1/household/rows/a%20b" || up.Header.Get("If-Match") != `"v1"` {
		t.Errorf("update upstream %s If-Match=%q", up.URL.EscapedPath(), up.Header.Get("If-Match"))
	}
	if resp, body := wr.do(t, "PATCH", "/writes/household/rows?key%5Bid%5D=1&key%5Bmonth%5D=2026-10", wr.a.GateBearer, `{"data":{}}`, nil); resp.StatusCode != 200 {
		t.Fatalf("update by query key: %d %s", resp.StatusCode, body)
	}
	up, _ = wr.api.last()
	if up.URL.Path != "/modules/api/app/v1/household/rows" || up.URL.Query().Get("key[id]") != "1" || up.URL.Query().Get("key[month]") != "2026-10" {
		t.Errorf("query key upstream %s ? %s", up.URL.Path, up.URL.RawQuery)
	}
}

func TestWrites_RefusesWhatTheOwnerDidntDeclare(t *testing.T) {
	wr := newWritesRig(t)
	for name, c := range map[string]struct{ method, path, bearer string }{
		"undeclared API":       {"POST", "/writes/payroll/rows", wr.a.GateBearer},
		"undeclared operation": {"DELETE", "/writes/household/rows/1", wr.a.GateBearer},
		"no declaration":       {"POST", "/writes/household/rows", wr.b.GateBearer},
	} {
		resp, body := wr.do(t, c.method, c.path, c.bearer, `{"data":{}}`, nil)
		if resp.StatusCode != 403 || errCode(t, body) != "not_a_write_target" {
			t.Errorf("%s: %d %s", name, resp.StatusCode, body)
		}
	}
	if n := wr.api.n(); n != 0 {
		t.Errorf("booth-api was called %d times for refused writes", n)
	}
}

func TestWrites_RefusesMalformedRequestsBeforeMinting(t *testing.T) {
	wr := newWritesRig(t)
	before := wr.m.n()
	for _, c := range []struct {
		method, path string
		code         int
	}{
		{"GET", "/writes/household/rows", 405},
		{"POST", "/writes/household/rows/1", 405},
		{"PATCH", "/writes/household/rows", 405},
		{"PUT", "/writes/household/rows/1", 405},
		{"POST", "/writes/household/graphql", 400},
		{"POST", "/writes/household/rows/1/x", 400},
		{"POST", "/writes/House/rows", 400},
		{"POST", "/writes/../rows", 400},
		{"PATCH", "/writes/household/rows/..", 400},
		{"PATCH", "/writes/household/rows/a%2fb", 400},
		{"PATCH", "/writes/household/rows/1?key%5Bid%5D=1", 400},
		{"POST", "/writes/household/rows?select=*", 400},
		{"PATCH", "/writes/household/rows?key%5Bid%5D=1&key%5Bid%5D=2", 400},
	} {
		resp, body := wr.do(t, c.method, c.path, wr.a.GateBearer, `{}`, nil)
		if resp.StatusCode != c.code {
			t.Errorf("%s %s: %d %s, want %d", c.method, c.path, resp.StatusCode, body, c.code)
		}
	}
	if wr.m.n() != before || wr.api.n() != 0 {
		t.Errorf("malformed requests minted %d tokens and made %d upstream calls", wr.m.n()-before, wr.api.n())
	}
}

func TestWrites_RefusesBadHeadersAndOversizeBodies(t *testing.T) {
	wr := newWritesRig(t)
	resp, _ := wr.do(t, "POST", "/writes/household/rows", wr.a.GateBearer, `{"data":{"x":"`+strings.Repeat("a", MaxWriteBodyBytes)+`"}}`, nil)
	if resp.StatusCode != 413 {
		t.Errorf("oversize: %d", resp.StatusCode)
	}
	for name, hdr := range map[string]map[string]string{
		"long idempotency key": {"Idempotency-Key": strings.Repeat("k", 256)},
		"control char user":    {"X-Booth-App-User": "a\tb"},
	} {
		if resp, body := wr.do(t, "POST", "/writes/household/rows", wr.a.GateBearer, `{"data":{}}`, hdr); resp.StatusCode != 400 {
			t.Errorf("%s: %d %s", name, resp.StatusCode, body)
		}
	}
	if wr.api.n() != 0 {
		t.Errorf("booth-api was called %d times", wr.api.n())
	}
}

func TestWrites_IdentityComesFromTheBearer(t *testing.T) {
	wr := newWritesRig(t)
	if resp, _ := wr.do(t, "POST", "/writes/household/rows", "", `{}`, nil); resp.StatusCode != 401 {
		t.Errorf("no bearer: %d", resp.StatusCode)
	}
	if resp, _ := wr.do(t, "POST", "/writes/household/rows", strings.Repeat("0", 64), `{}`, nil); resp.StatusCode != 401 {
		t.Errorf("unknown bearer: %d", resp.StatusCode)
	}
	// A paused owner: refused with the reason, nothing forwarded.
	wr.m.refuse["alice"] = true
	resp, body := wr.do(t, "POST", "/writes/household/rows", wr.a.GateBearer, `{"data":{}}`, nil)
	if resp.StatusCode != 403 || !strings.Contains(body, "data_access_paused") || wr.api.n() != 0 {
		t.Errorf("paused: %d %s, upstream calls %d", resp.StatusCode, body, wr.api.n())
	}
}

func TestWrites_RelaysBoothAPIRefusalsAndExplainsNotGranted(t *testing.T) {
	wr := newWritesRig(t)
	wr.api.status, wr.api.body = 422, `{"error":{"code":"validation","message":"row is invalid","fields":[{"field":"amount","problem":"required"}]}}`
	resp, body := wr.do(t, "POST", "/writes/household/rows", wr.a.GateBearer, `{"data":{}}`, nil)
	if resp.StatusCode != 422 || body != wr.api.body {
		t.Errorf("validation relayed as %d %s", resp.StatusCode, body)
	}
	wr.api.status, wr.api.body, wr.api.hdr = 429, `{"error":{"code":"rate_limited","message":"slow down"}}`, map[string]string{"Retry-After": "1"}
	if resp, _ := wr.do(t, "POST", "/writes/household/rows", wr.a.GateBearer, `{"data":{}}`, nil); resp.StatusCode != 429 || resp.Header.Get("Retry-After") != "1" {
		t.Errorf("rate limit relayed as %d Retry-After=%q", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
	wr.api.hdr = nil
	wr.api.status, wr.api.body = 403, `{"error":{"code":"not_granted","message":"no live grant"}}`
	resp, body = wr.do(t, "POST", "/writes/household/rows", wr.a.GateBearer, `{"data":{}}`, nil)
	if resp.StatusCode != 403 || errCode(t, body) != "not_granted" ||
		!strings.Contains(body, "also needs an owner's grant on the API page") || !strings.Contains(body, apps.Principal("acme", wr.a.ID)) {
		t.Errorf("not_granted: %d %s", resp.StatusCode, body)
	}
	// Any other 403 is relayed unchanged.
	wr.api.body = `{"error":{"code":"granting_owner_no_access","message":"the granting owner lost access"}}`
	if _, body := wr.do(t, "POST", "/writes/household/rows", wr.a.GateBearer, `{"data":{}}`, nil); body != wr.api.body {
		t.Errorf("other 403 rewritten: %s", body)
	}
}

func TestWrites_UnreachableBoothAPI(t *testing.T) {
	wr := newWritesRig(t)
	wr.api.srv.Close()
	resp, body := wr.do(t, "POST", "/writes/household/rows", wr.a.GateBearer, `{"data":{}}`, nil)
	if resp.StatusCode != 502 || errCode(t, body) != "unavailable" {
		t.Errorf("unreachable: %d %s", resp.StatusCode, body)
	}
}

func TestGrantRevoker(t *testing.T) {
	r := newRig(t)
	api := newFakeAPI(t)
	api.status, api.body = 200, `{"revoked":2,"items":[]}`
	g := &GrantRevoker{GatewayURL: api.srv.URL, Minter: r.m}
	if err := g.RevokeGrants(context.Background(), "acme", "streamlit:acme:aabc", "carol"); err != nil {
		t.Fatal(err)
	}
	up, body := api.last()
	if up.Method != "POST" || up.URL.Path != "/modules/api/api/write-grants/revoke-principal" || up.Header.Get("X-Workspace") != "acme" {
		t.Errorf("upstream %s %s ws=%q", up.Method, up.URL.Path, up.Header.Get("X-Workspace"))
	}
	if body != `{"principal":"streamlit:acme:aabc"}` {
		t.Errorf("body %s", body)
	}
	// Its own token: streamlit:<ws>:grants, minted under the owner taking over, capped at viewer.
	if sub, _ := unverifiedSubject(strings.TrimPrefix(up.Header.Get("Authorization"), "Bearer ")); sub != "streamlit:acme:grants" {
		t.Errorf("revoked with a token for %q", sub)
	}
	if last := r.m.calls[len(r.m.calls)-1]; last != "acme|streamlit:acme:grants|carol" {
		t.Errorf("minted %q", last)
	}

	// Fail closed on every failure.
	for _, c := range []struct {
		name  string
		setup func()
	}{
		{"non-2xx", func() { api.status, api.body = 401, `{"error":"unauthorized"}` }},
		{"server error", func() { api.status, api.body = 503, `{"error":"down"}` }},
		{"mint refused", func() { api.status = 200; r.m.refuse["carol"] = true }},
		{"role exceeded", func() { r.m.refuse["carol"] = false; r.m.role = "owner" }},
	} {
		c.setup()
		if err := g.RevokeGrants(context.Background(), "acme", "streamlit:acme:aabc", "carol"); err == nil {
			t.Errorf("%s: no error", c.name)
		}
	}
	r.m.role = ""
	api.srv.Close()
	if err := g.RevokeGrants(context.Background(), "acme", "streamlit:acme:aabc", "carol"); err == nil || !strings.Contains(err.Error(), "could not be reached") {
		t.Errorf("unreachable: %v", err)
	}
}

func TestGrantsSubjectIsNeverAnAppSubject(t *testing.T) {
	s := GrantsSubject("acme")
	if s != "streamlit:acme:grants" {
		t.Fatal(s)
	}
	// App ids are "a" + 12 base-32 characters; "grants" is 6 characters.
	r := newRig(t)
	a := r.app(t, "acme", "o")
	if len(a.ID) != 13 || Subject("acme", a.ID) == s {
		t.Errorf("app id %q", a.ID)
	}
}
