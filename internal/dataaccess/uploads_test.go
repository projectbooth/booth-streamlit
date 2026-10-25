package dataaccess

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/projectbooth/booth-streamlit/internal/apps"
	"github.com/projectbooth/booth-streamlit/internal/identity"
)

type uploadsRig struct {
	*rig
	storage *fakeAPI
	srv     *httptest.Server
	a       apps.App // acme, declares local:finance/inbox/
	b       apps.App // other-team, declares nothing
}

func newUploadsRig(t *testing.T, maxBytes int64) *uploadsRig {
	t.Helper()
	r := newRig(t)
	st := newFakeAPI(t)
	st.status, st.body = 200, `{"path":"ignored","size":3}`
	in := &Internal{Apps: r.svc, Tokens: r.tok, Uploads: &Uploads{GatewayURL: st.srv.URL, MaxBytes: maxBytes, MaxConcurrent: 2}}
	u := &uploadsRig{rig: r, storage: st}
	u.a = r.app(t, "acme", "alice")
	var err error
	u.a, err = r.svc.SetWriteTargets(context.Background(), identity.Caller{Subject: "alice", Workspace: "acme", Role: identity.RoleOwner}, u.a.ID,
		apps.WriteTargetsInput{Uploads: []apps.UploadTarget{{Backend: "local", Prefix: "finance/inbox"}}, Confirmed: true})
	if err != nil {
		t.Fatal(err)
	}
	u.b = r.app(t, "other-team", "olga")
	u.srv = httptest.NewServer(in.Router())
	t.Cleanup(u.srv.Close)
	return u
}

func (u *uploadsRig) do(t *testing.T, method, rawPath, bearer, body string, hdr map[string]string) (*http.Response, string) {
	t.Helper()
	return (&writesRig{srv: u.srv}).do(t, method, rawPath, bearer, body, hdr)
}

var storedPattern = regexp.MustCompile(`^finance/inbox/\d{8}T\d{6}Z-[a-z0-9]{6}-statement\.csv$`)

func TestUploads_CreateOnlyWriteWithTheAppsOwnToken(t *testing.T) {
	u := newUploadsRig(t, 0)
	resp, body := u.do(t, "POST", "/uploads/local?prefix=finance/inbox/&name=statement.csv", u.a.GateBearer, "a,b", map[string]string{
		"Content-Type": "text/csv", "X-Workspace": "other-team", "Cookie": "booth_iframe_session=stolen",
	})
	if resp.StatusCode != 201 {
		t.Fatalf("upload: %d %s", resp.StatusCode, body)
	}
	up, upBody := u.storage.last()
	path, ok := strings.CutPrefix(up.URL.Path, "/modules/storage/api/backends/local/objects/")
	if up.Method != "PUT" || !ok || !storedPattern.MatchString(path) || upBody != "a,b" {
		t.Fatalf("upstream %s %s %q", up.Method, up.URL.Path, upBody)
	}
	// The stored name is returned to the app.
	if !strings.Contains(body, `"path":"`+path+`"`) || !strings.Contains(body, `"backend":"local"`) || !strings.Contains(body, `"size":3`) {
		t.Errorf("answer %s, stored %s", body, path)
	}
	// Create-only, the app's existing token, the app's workspace; nothing of the caller's.
	if up.Header.Get("If-None-Match") != "*" || up.Header.Get("X-Workspace") != "acme" || up.Header.Get("Cookie") != "" || up.Header.Get("Content-Type") != "text/csv" {
		t.Errorf("upstream headers %v", up.Header)
	}
	jwt := strings.TrimPrefix(up.Header.Get("Authorization"), "Bearer ")
	if cached, _ := u.tok.Get(context.Background(), u.a); jwt != cached.JWT {
		t.Error("forwarded a different token from the app's cached one")
	}
	// Two uploads of one name never collide.
	u.do(t, "POST", "/uploads/local?prefix=finance/inbox/&name=statement.csv", u.a.GateBearer, "a,b", nil)
	again, _ := u.storage.last()
	if again.URL.Path == up.URL.Path {
		t.Errorf("the same stored name twice: %s", again.URL.Path)
	}
	if again.Header.Get("Content-Type") != "application/octet-stream" {
		t.Errorf("no Content-Type sent as %q", again.Header.Get("Content-Type"))
	}
	// Names are escaped into one segment.
	u.do(t, "POST", "/uploads/local?prefix=finance/inbox/&name=a%20b%3F%23.csv", u.a.GateBearer, "x", nil)
	odd, _ := u.storage.last()
	if !strings.HasSuffix(odd.URL.EscapedPath(), "-a%20b%3F%23.csv") || strings.Count(odd.URL.Path, "/") != strings.Count(up.URL.Path, "/") {
		t.Errorf("odd name upstream %s", odd.URL.EscapedPath())
	}
}

func TestUploads_RefusesWhatTheOwnerDidntDeclare(t *testing.T) {
	u := newUploadsRig(t, 0)
	for name, c := range map[string]struct{ path, bearer string }{
		"another backend": {"/uploads/s3?prefix=finance/inbox/&name=a.csv", u.a.GateBearer},
		"another folder":  {"/uploads/local?prefix=finance/&name=a.csv", u.a.GateBearer},
		"a deeper folder": {"/uploads/local?prefix=finance/inbox/2026/&name=a.csv", u.a.GateBearer},
		"the root":        {"/uploads/local?name=a.csv", u.a.GateBearer},
		"no declaration":  {"/uploads/local?prefix=finance/inbox/&name=a.csv", u.b.GateBearer},
	} {
		resp, body := u.do(t, "POST", c.path, c.bearer, "x", nil)
		if resp.StatusCode != 403 || errCode(t, body) != "not_an_upload_target" {
			t.Errorf("%s: %d %s", name, resp.StatusCode, body)
		}
	}
	if n := u.storage.n(); n != 0 {
		t.Errorf("booth-storage was called %d times for refused uploads", n)
	}
}

func TestUploads_RefusesMalformedRequestsBeforeMinting(t *testing.T) {
	u := newUploadsRig(t, 0)
	before := u.m.n()
	for _, c := range []struct {
		method, path string
		code         int
	}{
		{"GET", "/uploads/local?prefix=finance/inbox/&name=a.csv", 405},
		{"PUT", "/uploads/local?prefix=finance/inbox/&name=a.csv", 405},
		{"DELETE", "/uploads/local?prefix=finance/inbox/&name=a.csv", 405},
		{"POST", "/uploads/local/finance?name=a.csv", 400},
		{"POST", "/uploads/lo%2Fcal?prefix=finance/inbox/&name=a.csv", 400},
		{"POST", "/uploads/local?prefix=finance/inbox/", 400},
		{"POST", "/uploads/local?prefix=finance/inbox/&name=a/b.csv", 400},
		{"POST", "/uploads/local?prefix=finance/inbox/&name=..%5Ca.csv", 400},
		{"POST", "/uploads/local?prefix=finance/inbox/&name=.env", 400},
		{"POST", "/uploads/local?prefix=finance/inbox/&name=a%00.csv", 400},
		{"POST", "/uploads/local?prefix=finance/../inbox/&name=a.csv", 400},
		{"POST", "/uploads/local?prefix=finance//inbox/&name=a.csv", 400},
		{"POST", "/uploads/local?prefix=finance/inbox/&name=a.csv&name=b.csv", 400},
		{"POST", "/uploads/local?prefix=finance/inbox/&name=a.csv&overwrite=true", 400},
	} {
		resp, body := u.do(t, c.method, c.path, u.a.GateBearer, "x", nil)
		if resp.StatusCode != c.code {
			t.Errorf("%s %s: %d %s, want %d", c.method, c.path, resp.StatusCode, body, c.code)
		}
	}
	if u.m.n() != before || u.storage.n() != 0 {
		t.Errorf("malformed requests minted %d tokens and made %d upstream calls", u.m.n()-before, u.storage.n())
	}
}

func TestUploads_ModuleCeiling(t *testing.T) {
	u := newUploadsRig(t, 4)
	if resp, body := u.do(t, "POST", "/uploads/local?prefix=finance/inbox/&name=a.csv", u.a.GateBearer, "12345", nil); resp.StatusCode != 413 || errCode(t, body) != "too_large" {
		t.Errorf("over the ceiling: %d %s", resp.StatusCode, body)
	}
	if u.storage.n() != 0 {
		t.Error("an upload over the ceiling with a known length reached booth-storage")
	}
	if resp, body := u.do(t, "POST", "/uploads/local?prefix=finance/inbox/&name=a.csv", u.a.GateBearer, "1234", nil); resp.StatusCode != 201 {
		t.Errorf("at the ceiling: %d %s", resp.StatusCode, body)
	}
}

func TestUploads_ExplainsBoothStorageRefusals(t *testing.T) {
	u := newUploadsRig(t, 0)
	for _, c := range []struct {
		status int
		code   string
		says   string
	}{
		{403, "not_granted", "also needs an owner's grant on the Storage page"},
		{413, "too_large", "larger than this app's upload grant allows"},
		{415, "extension_not_allowed", "doesn't allow files of this type"},
		{412, "exists", "nothing was replaced"},
		{501, "create_only_unsupported", "can't yet guarantee create-only writes"},
		{404, "not_found", "was not found in this workspace"},
		{500, "upstream_error", "answered 500"},
	} {
		u.storage.status, u.storage.body = c.status, `{"error":"storage says no"}`
		resp, body := u.do(t, "POST", "/uploads/local?prefix=finance/inbox/&name=a.csv", u.a.GateBearer, "x", nil)
		if resp.StatusCode != c.status || errCode(t, body) != c.code || !strings.Contains(body, c.says) {
			t.Errorf("%d: %d %s", c.status, resp.StatusCode, body)
		}
	}
	u.storage.status = 403
	_, body := u.do(t, "POST", "/uploads/local?prefix=finance/inbox/&name=a.csv", u.a.GateBearer, "x", nil)
	if !strings.Contains(body, apps.Principal("acme", u.a.ID)) {
		t.Errorf("not_granted doesn't name the principal to grant: %s", body)
	}
}

func TestUploads_IdentityAndUnreachable(t *testing.T) {
	u := newUploadsRig(t, 0)
	if resp, _ := u.do(t, "POST", "/uploads/local?prefix=finance/inbox/&name=a.csv", "", "x", nil); resp.StatusCode != 401 {
		t.Errorf("no bearer: %d", resp.StatusCode)
	}
	u.m.refuse["alice"] = true
	resp, body := u.do(t, "POST", "/uploads/local?prefix=finance/inbox/&name=a.csv", u.a.GateBearer, "x", nil)
	if resp.StatusCode != 403 || !strings.Contains(body, "data_access_paused") || u.storage.n() != 0 {
		t.Errorf("paused: %d %s, upstream calls %d", resp.StatusCode, body, u.storage.n())
	}
	u.m.refuse["alice"] = false
	u.clock = u.clock.Add(RefusalMemory)
	u.storage.srv.Close()
	resp, body = u.do(t, "POST", "/uploads/local?prefix=finance/inbox/&name=a.csv", u.a.GateBearer, "x", nil)
	if resp.StatusCode != 502 || errCode(t, body) != "unavailable" {
		t.Errorf("unreachable: %d %s", resp.StatusCode, body)
	}
}

func TestStoredName(t *testing.T) {
	at := time.Date(2026, 10, 15, 9, 8, 7, 0, time.FixedZone("x", -5*3600))
	n := StoredName("finance/inbox/", "statement.csv", at)
	if !strings.HasPrefix(n, "finance/inbox/20261015T140807Z-") || !storedPattern.MatchString(n) {
		t.Errorf("StoredName = %s", n)
	}
}

func TestStorageRevoker(t *testing.T) {
	r := newRig(t)
	st := newFakeAPI(t)
	st.status, st.body = 200, `{"revoked":1,"items":[]}`
	g := &GrantRevoker{GatewayURL: st.srv.URL, Module: "booth-storage", Path: StorageRevokePath, Minter: r.m}
	if err := g.RevokeGrants(context.Background(), "acme", "streamlit:acme:aabc", "carol"); err != nil {
		t.Fatal(err)
	}
	up, body := st.last()
	if up.URL.Path != "/modules/storage/api/admin/upload-grants/revoke-principal" || up.Header.Get("X-Workspace") != "acme" || body != `{"principal":"streamlit:acme:aabc"}` {
		t.Errorf("upstream %s ws=%q %s", up.URL.Path, up.Header.Get("X-Workspace"), body)
	}
	if sub, _ := unverifiedSubject(strings.TrimPrefix(up.Header.Get("Authorization"), "Bearer ")); sub != "streamlit:acme:grants" {
		t.Errorf("revoked with a token for %q", sub)
	}
	st.status = 403
	if err := g.RevokeGrants(context.Background(), "acme", "streamlit:acme:aabc", "carol"); err == nil || !strings.Contains(err.Error(), "booth-storage answered 403") {
		t.Errorf("refused: %v", err)
	}
}

// A body of unknown length is cut off at the ceiling while it streams.
func TestUploads_ModuleCeilingWhileStreaming(t *testing.T) {
	u := newUploadsRig(t, 4)
	req, _ := http.NewRequest("POST", u.srv.URL+"/uploads/local?prefix=finance/inbox/&name=a.csv", io.MultiReader(strings.NewReader("123"), strings.NewReader("45")))
	req.ContentLength = -1
	req.Header.Set("Authorization", "Bearer "+u.a.GateBearer)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 413 || errCode(t, string(b)) != "too_large" {
		t.Errorf("streamed over the ceiling: %d %s", resp.StatusCode, b)
	}
}
