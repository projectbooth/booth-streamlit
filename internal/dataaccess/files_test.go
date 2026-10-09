package dataaccess

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/projectbooth/booth-streamlit/internal/apps"
)

// fakeGateway stands in for booth-core's gateway in front of booth-storage and booth-catalog, as
// workspace-scoped as the real ones: it serves acme's backend "files" and dataset "ds1" only to
// X-Workspace: acme, and other-team's to other-team. It records what it was asked.
type fakeGateway struct {
	mu   sync.Mutex
	seen []*http.Request
	srv  *httptest.Server
	big  int64 // if set, objects report this Content-Length
	hold chan struct{}
}

func newGateway(t *testing.T) *fakeGateway {
	t.Helper()
	g := &fakeGateway{}
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		g.seen = append(g.seen, r.Clone(context.Background()))
		hold := g.hold
		g.mu.Unlock()
		if hold != nil {
			<-hold
		}
		ws := r.Header.Get("X-Workspace")
		objects := map[string]map[string]string{
			"acme":       {"files": "sales/q1.csv sales/q2.csv other/secret.csv"},
			"other-team": {"other-files": "theirs/x.csv"},
		}
		switch {
		case r.URL.Path == "/modules/catalog/api/datasets/ds1" && ws == "acme":
			_, _ = w.Write([]byte(`{"id":"ds1","name":"Sales","description":"","format":"file","location":{"backendId":"files","path":"sales"},"schema":[],"owner":"o"}`))
		case r.URL.Path == "/modules/catalog/api/datasets/tbl" && ws == "acme":
			_, _ = w.Write([]byte(`{"id":"tbl","name":"T","format":"iceberg","location":{"backendId":"lake","path":"w/t"}}`))
		case strings.HasPrefix(r.URL.Path, "/modules/storage/api/backends/"):
			rest := strings.TrimPrefix(r.URL.Path, "/modules/storage/api/backends/")
			backend, obj, _ := strings.Cut(rest, "/objects")
			keys, ok := objects[ws][backend]
			if !ok {
				http.Error(w, "backend not found", 404)
				return
			}
			obj = strings.TrimPrefix(obj, "/")
			if obj == "" {
				_ = json.NewEncoder(w).Encode(map[string]any{"entries": strings.Fields(keys), "query": r.URL.RawQuery})
				return
			}
			if !strings.Contains(" "+keys+" ", " "+obj+" ") {
				http.Error(w, "object not found", 404)
				return
			}
			w.Header().Set("Content-Type", "text/csv")
			w.Header().Set("Content-Disposition", `attachment; filename="x.csv"`)
			w.Header().Set("X-Content-Type-Options", "nosniff")
			body := "content of " + obj
			if g.big > 0 {
				w.Header().Set("Content-Length", strconv.FormatInt(g.big, 10))
			}
			_, _ = w.Write([]byte(body))
		default:
			http.Error(w, "not found", 404)
		}
	}))
	t.Cleanup(g.srv.Close)
	return g
}

func (g *fakeGateway) last() *http.Request {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.seen) == 0 {
		return nil
	}
	return g.seen[len(g.seen)-1]
}

type filesRig struct {
	*rig
	gw  *fakeGateway
	h   http.Handler
	a   apps.App // acme, owner alice
	b   apps.App // other-team, owner olga
	srv *httptest.Server
}

func newFilesRig(t *testing.T) *filesRig {
	t.Helper()
	r := newRig(t)
	gw := newGateway(t)
	in := &Internal{Apps: r.svc, Tokens: r.tok, Files: &Files{GatewayURL: gw.srv.URL, MaxConcurrent: 2}}
	fr := &filesRig{rig: r, gw: gw, h: in.Router()}
	fr.a = r.app(t, "acme", "alice")
	fr.b = r.app(t, "other-team", "olga")
	fr.srv = httptest.NewServer(fr.h)
	t.Cleanup(fr.srv.Close)
	return fr
}

// get sends a raw request (the path is not normalized by the client) with an app's bearer.
func (fr *filesRig) do(t *testing.T, method, rawPath, bearer string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, fr.srv.URL+"/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.URL.Opaque = "//" + req.URL.Host + rawPath // send the path exactly as written
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestFiles_ReadsAsTheAppThroughCoresGateway(t *testing.T) {
	fr := newFilesRig(t)
	code, body := fr.do(t, "GET", "/files/storage/files/sales/q1.csv", fr.a.GateBearer)
	if code != 200 || body != "content of sales/q1.csv" {
		t.Fatalf("read: %d %q", code, body)
	}
	up := fr.gw.last()
	if up.URL.Path != "/modules/storage/api/backends/files/objects/sales/q1.csv" ||
		up.Header.Get("X-Workspace") != "acme" ||
		!strings.HasPrefix(up.Header.Get("Authorization"), "Bearer ") {
		t.Errorf("upstream request %s ws=%q auth=%q", up.URL.Path, up.Header.Get("X-Workspace"), up.Header.Get("Authorization"))
	}
	if sub, _ := unverifiedSubject(strings.TrimPrefix(up.Header.Get("Authorization"), "Bearer ")); sub != Subject("acme", fr.a.ID) {
		t.Errorf("read with a token for %q, want app A's", sub)
	}

	code, body = fr.do(t, "GET", "/files/storage/files?prefix=sales/&recursive=true&limit=5000", fr.a.GateBearer)
	if code != 200 || !strings.Contains(body, "sales/q1.csv") {
		t.Fatalf("list: %d %s", code, body)
	}
	if q := fr.gw.last().URL.Query(); q.Get("limit") != "1000" || q.Get("prefix") != "sales/" || q.Get("recursive") != "true" {
		t.Errorf("list query %v: limit must be clamped to 1000", q)
	}

	code, body = fr.do(t, "GET", "/files/datasets/ds1", fr.a.GateBearer)
	if code != 200 || !strings.Contains(body, `"backendId":"files"`) || strings.Contains(body, `"owner"`) {
		t.Fatalf("dataset: %d %s", code, body)
	}
	code, body = fr.do(t, "GET", "/files/datasets/ds1/content/sales/q2.csv", fr.a.GateBearer)
	if code != 200 || body != "content of sales/q2.csv" {
		t.Fatalf("dataset content: %d %q", code, body)
	}
	if code, _ := fr.do(t, "HEAD", "/files/storage/files/sales/q1.csv", fr.a.GateBearer); code != 200 {
		t.Errorf("HEAD: %d", code)
	}
}

func TestFiles_Refusals(t *testing.T) {
	fr := newFilesRig(t)
	a := fr.a.GateBearer
	cases := []struct {
		name, method, path, bearer string
		want                       int
	}{
		{"no bearer", "GET", "/files/storage/files/sales/q1.csv", "", 401},
		{"unknown bearer", "GET", "/files/storage/files/sales/q1.csv", strings.Repeat("0", 64), 401},
		{"PUT", "PUT", "/files/storage/files/sales/q1.csv", a, 405},
		{"POST", "POST", "/files/storage/files", a, 405},
		{"DELETE", "DELETE", "/files/datasets/ds1", a, 405},
		{"dot-dot", "GET", "/files/storage/files/sales/../other/secret.csv", a, 400},
		{"encoded dot-dot", "GET", "/files/storage/files/sales/%2e%2e/other/secret.csv", a, 400},
		{"encoded slash", "GET", "/files/storage/files/sales%2F..%2Fother%2Fsecret.csv", a, 400},
		// Decodes to a valid path, so only the encoded-separator check can refuse it.
		{"encoded slash alone", "GET", "/files/storage/files/sales%2Fq1.csv", a, 400},
		{"encoded backslash", "GET", "/files/storage/files/sales%5Cq1.csv", a, 400},
		{"empty segment", "GET", "/files/storage/files/sales//q1.csv", a, 400},
		{"dot segment", "GET", "/files/storage/files/./sales/q1.csv", a, 400},
		{"NUL", "GET", "/files/storage/files/sales%00/q1.csv", a, 400},
		{"prefix with dot-dot", "GET", "/files/storage/files?prefix=../x", a, 400},
		{"outside the dataset's location", "GET", "/files/datasets/ds1/content/other/secret.csv", a, 403},
		{"prefix-sibling of the location", "GET", "/files/datasets/ds1/content/salesman/x.csv", a, 403},
		{"a table, not a file dataset", "GET", "/files/datasets/tbl/content/w/t/data.parquet", a, 400},
		{"another workspace's backend", "GET", "/files/storage/other-files/theirs/x.csv", a, 404},
		{"another workspace's dataset", "GET", "/files/datasets/theirs", a, 404},
		// App B (other-team) with its own bearer: acme's backend doesn't exist for it.
		{"B's bearer on A's data", "GET", "/files/storage/files/sales/q1.csv", fr.b.GateBearer, 404},
		{"B's bearer on A's dataset", "GET", "/files/datasets/ds1/content/sales/q1.csv", fr.b.GateBearer, 404},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if code, body := fr.do(t, c.method, c.path, c.bearer); code != c.want {
				t.Errorf("%s %s: %d %s, want %d", c.method, c.path, code, body, c.want)
			}
		})
	}
	// B's bearer does reach B's own data: it resolves to B, not to nothing.
	if code, _ := fr.do(t, "GET", "/files/storage/other-files/theirs/x.csv", fr.b.GateBearer); code != 200 {
		t.Errorf("B reading its own workspace's file: %d", code)
	}
}

func TestFiles_PausedOwnerGets403WithTheReason(t *testing.T) {
	fr := newFilesRig(t)
	fr.m.refuse["alice"] = true
	code, body := fr.do(t, "GET", "/files/storage/files/sales/q1.csv", fr.a.GateBearer)
	if code != 403 || !strings.Contains(body, "data_access_paused") || !strings.Contains(body, "no longer has access") {
		t.Fatalf("paused owner: %d %s", code, body)
	}
	if got, _ := fr.store.Get(context.Background(), "acme", fr.a.ID); got.DataPausedReason == "" {
		t.Error("the pause was not recorded")
	}
	if n := len(fr.gw.seen); n != 0 {
		t.Errorf("a paused app's request reached the gateway (%d calls)", n)
	}
}

func TestFiles_SizeLimitAndConcurrency(t *testing.T) {
	fr := newFilesRig(t)
	fr.gw.big = DefaultMaxObjectBytes + 1
	if code, _ := fr.do(t, "GET", "/files/storage/files/sales/q1.csv", fr.a.GateBearer); code != http.StatusRequestEntityTooLarge {
		t.Errorf("object over the limit: %d, want 413", code)
	}
	fr.gw.big = 0

	// MaxConcurrent is 2 in this rig: hold two reads open, the third is refused.
	hold := make(chan struct{})
	fr.gw.mu.Lock()
	fr.gw.hold = hold
	fr.gw.mu.Unlock()
	done := make(chan int, 2)
	for i := 0; i < 2; i++ {
		go func() { c, _ := fr.do(t, "GET", "/files/storage/files/sales/q1.csv", fr.a.GateBearer); done <- c }()
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		fr.gw.mu.Lock()
		n := len(fr.gw.seen)
		fr.gw.mu.Unlock()
		if n >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if code, _ := fr.do(t, "GET", "/files/storage/files/sales/q1.csv", fr.a.GateBearer); code != http.StatusTooManyRequests {
		t.Errorf("third concurrent read: %d, want 429", code)
	}
	// Another app is not limited by A's reads.
	fr.gw.mu.Lock()
	fr.gw.hold = nil
	fr.gw.mu.Unlock()
	close(hold)
	for i := 0; i < 2; i++ {
		if c := <-done; c != 200 {
			t.Errorf("held read: %d", c)
		}
	}
}

func TestCleanPath(t *testing.T) {
	for _, ok := range []string{"a", "a/b.csv", "dir/sub/x y.parquet"} {
		if _, err := cleanPath(ok); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "/a", "a/../b", "..", "a/./b", "a//b", "a/", "a\\b", "a\x00b", strings.Repeat("a", maxPathLen+1)} {
		if _, err := cleanPath(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
