package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"

	"github.com/projectbooth/booth-streamlit/internal/identity"
)

// fixed is a Resolver over a fixed table, checking only the workspace.
type fixed map[string]App

func (f fixed) Resolve(_ context.Context, c identity.Caller, id string) (App, error) {
	a, ok := f[id]
	if !ok || a.Workspace != c.Workspace {
		return App{}, ErrNotFound
	}
	return a, nil
}

// fakeVerifier accepts the request iff it carries X-Booth-Identity: good-<workspace>.
type fakeVerifier struct{}

func (fakeVerifier) Verify(r *http.Request) (identity.Caller, error) {
	v := r.Header.Get(identity.HeaderIdentity)
	switch {
	case v == "":
		return identity.Caller{}, identity.ErrMissing
	case v == "forbidden":
		return identity.Caller{}, identity.ErrForbidden
	case strings.HasPrefix(v, "good-"):
		return identity.Caller{Subject: "user-1", Workspace: strings.TrimPrefix(v, "good-"), Role: identity.RoleViewer}, nil
	}
	return identity.Caller{}, identity.ErrInvalid
}

// app is a stand-in Streamlit: it records what it received and can answer a websocket.
type app struct {
	srv  *httptest.Server
	last *http.Request
}

func newApp(t *testing.T) *app {
	t.Helper()
	a := &app{}
	a.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.last = r.Clone(context.Background())
		if r.URL.Path == "/apps/demo/_stcore/stream" {
			c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
			if err != nil {
				return
			}
			defer c.CloseNow()
			typ, msg, err := c.Read(r.Context())
			if err == nil {
				_ = c.Write(r.Context(), typ, append([]byte("echo:"), msg...))
			}
			return
		}
		http.SetCookie(w, &http.Cookie{Name: CoreSessionCookie, Value: "overwritten", Path: "/"})
		http.SetCookie(w, &http.Cookie{Name: "_streamlit_xsrf", Value: "x", Path: "/"})
		_, _ = w.Write([]byte("app:" + r.URL.Path))
	}))
	t.Cleanup(a.srv.Close)
	return a
}

func newProxy(t *testing.T, a *app) *httptest.Server {
	t.Helper()
	u, _ := url.Parse(a.srv.URL)
	r := chi.NewRouter()
	(&Handler{Verifier: fakeVerifier{}, Apps: fixed{"demo": {ID: "demo", Workspace: "acme", Target: u, Bearer: "the-bearer"}}}).Mount(r)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv
}

func do(t *testing.T, srv *httptest.Server, path string, hdr map[string]string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+path, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestProxy_ForwardsAVerifiedViewerWithOnlyVerifiedHeaders(t *testing.T) {
	a := newApp(t)
	srv := newProxy(t, a)
	resp := do(t, srv, "/apps/demo/static/x.js", map[string]string{
		identity.HeaderIdentity: "good-acme",
		"X-Booth-Workspace":     "acme",
		"X-Booth-Role":          "viewer",
		"X-Booth-User":          "spoofed",
		"X-Booth-Anything":      "spoofed",
		"Authorization":         "Bearer something",
		"Cookie":                "booth_iframe_session=SECRET; _streamlit_xsrf=keep; other=1",
	})
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	got := a.last
	if got.URL.Path != "/apps/demo/static/x.js" {
		t.Errorf("path forwarded as %q; Streamlit's baseUrlPath expects it unchanged", got.URL.Path)
	}
	if got.Header.Get(identity.HeaderIdentity) != "" {
		t.Error("X-Booth-Identity reached the app: its code could replay the viewer's assertion")
	}
	if strings.Contains(got.Header.Get("Cookie"), "SECRET") || strings.Contains(got.Header.Get("Cookie"), CoreSessionCookie) {
		t.Errorf("core's session cookie reached the app: %q", got.Header.Get("Cookie"))
	}
	if c := got.Header.Get("Cookie"); c != "_streamlit_xsrf=keep; other=1" {
		t.Errorf("the app's own cookies were not kept intact: %q", c)
	}
	if got.Header.Get("Authorization") != "" || got.Header.Get("X-Booth-Anything") != "" {
		t.Error("an unverified header reached the app")
	}
	for k, want := range map[string]string{HeaderUser: "user-1", HeaderWorkspace: "acme", HeaderRole: "viewer"} {
		if got.Header.Get(k) != want {
			t.Errorf("%s = %q, want %q (set from the verified caller, not the request)", k, got.Header.Get(k), want)
		}
	}
	if got.Host != strings.TrimPrefix(srv.URL, "http://") {
		t.Errorf("Host = %q; the browser's Host must be kept for Streamlit's origin check", got.Host)
	}
	setCookies := strings.Join(resp.Header.Values("Set-Cookie"), "\n")
	if strings.Contains(setCookies, CoreSessionCookie) {
		t.Errorf("the app was allowed to set core's session cookie: %s", setCookies)
	}
	if !strings.Contains(setCookies, "_streamlit_xsrf") {
		t.Errorf("the app's own cookie was dropped: %s", setCookies)
	}
}

func TestProxy_Refusals(t *testing.T) {
	a := newApp(t)
	srv := newProxy(t, a)
	cases := []struct {
		name, path, assertion string
		want                  int
	}{
		{"no identity", "/apps/demo/", "", 401},
		{"invalid identity", "/apps/demo/", "bad", 401},
		{"no role in workspace", "/apps/demo/", "forbidden", 403},
		// Sharing is the app's own workspace only; another workspace looks like a missing app.
		{"member of another workspace", "/apps/demo/", "good-other", 404},
		{"unknown app", "/apps/nope/", "good-acme", 404},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a.last = nil
			resp := do(t, srv, c.path, map[string]string{identity.HeaderIdentity: c.assertion})
			if resp.StatusCode != c.want {
				t.Errorf("status = %d, want %d", resp.StatusCode, c.want)
			}
			if a.last != nil {
				t.Error("a refused request reached the app")
			}
		})
	}
}

func TestProxy_RedirectsToTheTrailingSlash(t *testing.T) {
	srv := newProxy(t, newApp(t))
	resp := do(t, srv, "/apps/demo?embed=true", nil)
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/apps/demo/?embed=true" {
		t.Errorf("got %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
}

// Streamlit's session runs over a websocket at <base>/_stcore/stream; it must pass through, and
// the upgrade request is identity-checked like any other.
func TestProxy_Websocket(t *testing.T) {
	srv := newProxy(t, newApp(t))
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/apps/demo/_stcore/stream"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: http.Header{identity.HeaderIdentity: {"good-acme"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	if err := c.Write(ctx, websocket.MessageBinary, []byte("hi")); err != nil {
		t.Fatal(err)
	}
	_, msg, err := c.Read(ctx)
	if err != nil || string(msg) != "echo:hi" {
		t.Fatalf("read %q, %v", msg, err)
	}

	_, resp, err := websocket.Dial(ctx, wsURL, nil)
	if err == nil || resp == nil || resp.StatusCode != 401 {
		t.Fatalf("an unauthenticated upgrade was not refused: %v %v", resp, err)
	}
}

// disclosed is a Disclosure over a fixed set of app ids.
type disclosed map[string]bool

func (d disclosed) Disclosed(_ context.Context, _ identity.Caller, id string) bool { return d[id] }

// An app that can write shows the module page's header on every visit (ADR 0117 item 2): any page
// load other than the module page's own frame goes to that page; the page's own requests don't.
func TestProxy_SendsPageLoadsOfADisclosedAppToTheWrappedView(t *testing.T) {
	a := newApp(t)
	u, _ := url.Parse(a.srv.URL)
	r := chi.NewRouter()
	(&Handler{
		Verifier:   fakeVerifier{},
		Apps:       fixed{"demo": {ID: "demo", Workspace: "acme", Target: u}, "plain": {ID: "plain", Workspace: "acme", Target: u}},
		Disclosure: disclosed{"demo": true},
	}).Mount(r)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)

	id := identity.HeaderIdentity
	nav := func(dest, referer string) map[string]string {
		h := map[string]string{id: "good-acme", "Sec-Fetch-Mode": "navigate", "Sec-Fetch-Dest": dest}
		if referer != "" {
			h["Referer"] = referer
		}
		return h
	}
	const shell = "https://booth.example/iframe/streamlit/"
	cases := []struct {
		name, path string
		hdr        map[string]string
		location   string // "" means forwarded to the app
	}{
		{"top-level load", "/apps/demo/", nav("document", ""), "../../#app=demo"},
		{"top-level load of a page", "/apps/demo/sub/page", nav("document", shell), "../../../#app=demo"},
		{"top-level, no fetch metadata", "/apps/demo/", map[string]string{id: "good-acme", "Accept": "text/html"}, "../../#app=demo"},
		{"frame the shell points at the app", "/apps/demo/", nav("iframe", "https://booth.example/"), "../../#app=demo"},
		{"frame without a referrer", "/apps/demo/", nav("iframe", ""), "../../#app=demo"},
		{"frame inside another app", "/apps/demo/", nav("iframe", shell+"apps/other/"), "../../#app=demo"},
		{"the module page's frame", "/apps/demo/", nav("iframe", shell), ""},
		{"the app's own page reloading", "/apps/demo/", nav("iframe", shell+"apps/demo/"), ""},
		{"the page's scripts", "/apps/demo/static/app.js", map[string]string{id: "good-acme", "Sec-Fetch-Mode": "no-cors", "Sec-Fetch-Dest": "script"}, ""},
		{"an app without targets", "/apps/plain/", nav("document", ""), ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a.last = nil
			resp := do(t, srv, c.path, c.hdr)
			if c.location == "" {
				if resp.StatusCode != 200 || a.last == nil {
					t.Fatalf("status %d, forwarded %v; want it forwarded", resp.StatusCode, a.last != nil)
				}
				return
			}
			if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != c.location {
				t.Errorf("got %d %q, want 303 %q", resp.StatusCode, resp.Header.Get("Location"), c.location)
			}
			if a.last != nil {
				t.Error("a redirected page load reached the app")
			}
		})
	}

	// The relative Location lands on the module page, whatever prefix core serves it under.
	base, _ := url.Parse("https://booth.example/iframe/streamlit/apps/demo/sub/page")
	if got := base.ResolveReference(&url.URL{Path: "../../../", Fragment: "app=demo"}).String(); got != shell+"#app=demo" {
		t.Errorf("resolves to %s", got)
	}
}
