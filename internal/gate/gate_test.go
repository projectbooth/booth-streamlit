package gate

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

const bearer = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// streamlit is a stand-in upstream that records what reached it.
func streamlit(t *testing.T) (*httptest.Server, *[]*http.Request) {
	t.Helper()
	var seen []*http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Clone(context.Background()))
		switch r.URL.Path {
		case "/apps/a1/_stcore/health":
			_, _ = w.Write([]byte("ok"))
		case "/apps/a1/_stcore/stream":
			c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
			if err != nil {
				return
			}
			defer c.CloseNow()
			if typ, msg, err := c.Read(r.Context()); err == nil {
				_ = c.Write(r.Context(), typ, append([]byte("echo:"), msg...))
			}
		default:
			_, _ = w.Write([]byte("page"))
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

func gateFor(t *testing.T, up *httptest.Server, b string) *httptest.Server {
	t.Helper()
	u, _ := url.Parse(up.URL)
	g := httptest.NewServer(New(Config{Bearer: b, Upstream: u, UpstreamHealth: "/apps/a1/_stcore/health"}))
	t.Cleanup(g.Close)
	return g
}

func get(t *testing.T, srv *httptest.Server, path string, hdr map[string]string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+path, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// The case the brief names: a request that forges X-Booth-User but has no bearer (or a wrong one)
// never reaches Streamlit.
func TestGate_RefusesWithoutTheBearer(t *testing.T) {
	up, seen := streamlit(t)
	g := gateFor(t, up, bearer)
	forged := map[string]string{"X-Booth-User": "someone-else", "X-Booth-Workspace": "acme", "X-Booth-Role": "owner"}
	for name, extra := range map[string]map[string]string{
		"no bearer":        {},
		"wrong bearer":     {HeaderToken: strings.Repeat("0", 64)},
		"truncated bearer": {HeaderToken: bearer[:32]},
		"empty bearer":     {HeaderToken: ""},
	} {
		hdr := map[string]string{}
		for k, v := range forged {
			hdr[k] = v
		}
		for k, v := range extra {
			hdr[k] = v
		}
		if code := get(t, g, "/apps/a1/", hdr); code != http.StatusUnauthorized {
			t.Errorf("%s: %d, want 401", name, code)
		}
	}
	if len(*seen) != 0 {
		t.Fatalf("%d refused request(s) reached Streamlit", len(*seen))
	}
}

func TestGate_ForwardsWithTheBearerAndNeverPassesItOn(t *testing.T) {
	up, seen := streamlit(t)
	g := gateFor(t, up, bearer)
	if code := get(t, g, "/apps/a1/static/x.js", map[string]string{HeaderToken: bearer, "X-Booth-User": "u1"}); code != 200 {
		t.Fatalf("status %d", code)
	}
	r := (*seen)[0]
	if r.Header.Get(HeaderToken) != "" {
		t.Error("the bearer reached Streamlit, where app code could read it")
	}
	if r.Header.Get("X-Booth-User") != "u1" || r.URL.Path != "/apps/a1/static/x.js" {
		t.Errorf("forwarded %s with X-Booth-User=%q", r.URL.Path, r.Header.Get("X-Booth-User"))
	}
	if r.Host != strings.TrimPrefix(g.URL, "http://") {
		t.Errorf("Host = %q; must be kept for Streamlit's websocket origin check", r.Host)
	}
}

func TestGate_EmptyBearerFailsClosed(t *testing.T) {
	up, _ := streamlit(t)
	g := gateFor(t, up, "")
	if code := get(t, g, "/apps/a1/", map[string]string{HeaderToken: ""}); code != http.StatusUnauthorized {
		t.Errorf("a gate with no bearer configured let a request through: %d", code)
	}
}

func TestGate_Health(t *testing.T) {
	up, seen := streamlit(t)
	if code := get(t, gateFor(t, up, bearer), HealthPath, nil); code != 200 {
		t.Errorf("health with Streamlit up: %d", code)
	}
	if len(*seen) != 1 || (*seen)[0].URL.Path != "/apps/a1/_stcore/health" {
		t.Errorf("health did not check Streamlit's own health path")
	}
	down, _ := url.Parse("http://127.0.0.1:1")
	g := httptest.NewServer(New(Config{Bearer: bearer, Upstream: down, UpstreamHealth: "/apps/a1/_stcore/health"}))
	defer g.Close()
	if code := get(t, g, HealthPath, nil); code != 503 {
		t.Errorf("health with Streamlit down: %d, want 503", code)
	}
}

func TestGate_Websocket(t *testing.T) {
	up, _ := streamlit(t)
	g := gateFor(t, up, bearer)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	wsURL := "ws" + strings.TrimPrefix(g.URL, "http") + "/apps/a1/_stcore/stream"

	c, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: http.Header{HeaderToken: {bearer}}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	_ = c.Write(ctx, websocket.MessageText, []byte("hi"))
	if _, msg, err := c.Read(ctx); err != nil || string(msg) != "echo:hi" {
		t.Fatalf("%q %v", msg, err)
	}
	if _, resp, err := websocket.Dial(ctx, wsURL, nil); err == nil || resp == nil || resp.StatusCode != 401 {
		t.Fatalf("an upgrade without the bearer was not refused: %v %v", resp, err)
	}
}
